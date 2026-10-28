#!/bin/sh
# M1 gate: init -> vaulted playbook against a lima node (redacted events) ->
# three-leg backup (restic_local, restic_s3 via SeaweedFS-under-podman, git) ->
# restore from every leg with the RECOVERY identity, canary decrypts on each.
# Run from the repo root. Requires: lima node template under test/lima/,
# ansible-playbook, restic, age, openssl, git, podman, curl. Never uses sudo.
set -eu

ROOT=$(git rev-parse --show-toplevel)
INSTANCE=${EXOCOMP_E2E_INSTANCE:-exocomp-node0}
WORK=$(mktemp -d)
SEAWEEDFS_NAME=""

cleanup() {
    if [ -n "$SEAWEEDFS_NAME" ]; then
        podman rm -f --time 1 "$SEAWEEDFS_NAME" >/dev/null 2>&1 || true
    fi
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export PATH="$HOME/.local/bin:$PATH"
# Self-skip on a host without the tools this gate drives, so a hosted run
# reports SKIP rather than a failure (see .github/workflows/e2e.yml). Before
# the podman() wrapper below shadows the binary.
for tool in limactl ansible-playbook ansible-vault restic age openssl git podman curl ssh; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'SKIP: %s absent\n' "$tool"; exit 0; }
done

# podman must keep the host's own storage and config: rootless storage under
# XDG_DATA_HOME would otherwise land in WORK, and subuid-owned layers there
# cannot be removed by a plain `rm -rf` at cleanup; XDG_CONFIG_HOME carries the
# host's registries and policy config.
podman() { env -u XDG_DATA_HOME -u XDG_CONFIG_HOME command podman "$@"; }

export XDG_DATA_HOME="$WORK/data"
# Hermetic client env: a hosts.toml default remote, an inherited sandbox
# tier, or an inherited output format would all change what these commands do.
export XDG_CONFIG_HOME="$WORK/config"
export EXOCOMP_REMOTE=
export EXOCOMP_SANDBOX=
export EXOCOMP_OUTPUT=
DATADIR="$XDG_DATA_HOME/exocomp"

log "build"
make -C "$ROOT" -s build
EXO="$ROOT/bin/exocomp"

log "init + capture recovery identity"
"$EXO" init >"$WORK/kit.txt"
grep -o 'AGE-SECRET-KEY-1[A-Z0-9]*' "$WORK/kit.txt" | head -1 >"$WORK/recovery.key"
[ -s "$WORK/recovery.key" ] || fail "no recovery key in kit output"

log "seal canary secret"
CANARY_VALUE="e2e-canary-$$"
printf '%s' "$CANARY_VALUE" | "$EXO" secrets add canary --kind e2e
"$EXO" secrets list | grep -q canary || fail "canary not listed"

log "derive vault password via the recovery-card path (age + openssl)"
DEK_HEX=$(age -d -i "$WORK/recovery.key" "$DATADIR/keyring.age" | od -An -tx1 | tr -d ' \n')
openssl kdf -keylen 32 -kdfopt digest:SHA256 -kdfopt hexkey:"$DEK_HEX" \
    -kdfopt info:ansible-vault:e2e -kdfopt salt:exocomp/v1 -binary HKDF \
    | base64 -w0 >"$WORK/vault-pass"
unset DEK_HEX

log "start lima node"
node_status=$(limactl list --format '{{.Name}} {{.Status}}' 2>/dev/null | awk -v n="$INSTANCE" '$1==n{print $2}')
case "$node_status" in
    Running) ;;
    "")
        limactl create --name "$INSTANCE" -y "$ROOT/test/lima/node-fedora.yaml" >"$WORK/lima.log" 2>&1             || { tail -5 "$WORK/lima.log"; fail "lima create failed"; }
        limactl start "$INSTANCE" >"$WORK/lima.log" 2>&1             || { tail -5 "$WORK/lima.log"; fail "lima start failed"; } ;;
    *)
        limactl start "$INSTANCE" >"$WORK/lima.log" 2>&1             || { tail -5 "$WORK/lima.log"; fail "lima start failed"; } ;;
esac
# Playbook, inventory, vaulted var, and ssh_config live under $WORK/src, not
# $WORK itself: a sandboxed job reads the playbook's and inventory's directory,
# and $WORK also holds the data dir ($WORK/data) — a source path may not be an
# ancestor of the data dir, so the sources get their own subtree.
SRC="$WORK/src"
mkdir -p "$SRC"
limactl show-ssh --format=config "$INSTANCE" >"$SRC/ssh_config"
NODE="lima-$INSTANCE"
cat >"$SRC/inventory.ini" <<EOF
[nodes]
$NODE
[nodes:vars]
ansible_ssh_common_args=-F $SRC/ssh_config
EOF

log "vaulted playbook against the node (canary value as the vaulted var)"
printf '%s' "$CANARY_VALUE" | ansible-vault encrypt_string \
    --vault-id exocomp@"$WORK/vault-pass" --stdin-name marker >"$SRC/vaulted-var.yml"
cat >"$SRC/play.yml" <<EOF
- hosts: nodes
  gather_facts: false
  vars_files:
    - $SRC/vaulted-var.yml
  tasks:
    - name: write the marker on the node
      ansible.builtin.copy:
        content: "{{ marker }}"
        dest: /tmp/exocomp-e2e
        mode: "0600"
    - name: print the marker locally, so redaction has something to catch
      ansible.builtin.debug:
        msg: "{{ marker }}"
EOF
# Reuse lima's persistent ssh control master (a pathname AF_UNIX socket, which
# the sandbox permits reaching) so the play connects without reading lima's
# identity key or dialing lima's dynamic TCP port — the sandbox grants a job
# neither, and a real fleet reaches its nodes on port 22 with a granted key.
export ANSIBLE_SSH_CONTROL_PATH="$HOME/.lima/$INSTANCE/ssh.sock"
"$EXO" playbook run "$SRC/play.yml" --inventory "$SRC/inventory.ini" \
    --vault-project e2e >"$WORK/play.out" 2>&1 || { cat "$WORK/play.out"; fail "playbook run failed"; }
limactl shell "$INSTANCE" -- cat /tmp/exocomp-e2e | grep -q "$CANARY_VALUE" \
    || fail "marker not written on node"

log "redaction: plaintext absent from the database, [REDACTED] present in events"
if grep -aq -- "$CANARY_VALUE" "$DATADIR/exocomp.db"; then
    fail "vaulted value leaked into exocomp.db"
fi
grep -q '\[REDACTED\]' "$WORK/play.out" \
    || fail "redaction marker absent from the job's captured events"

log "start SeaweedFS under podman for the restic_s3 leg"
SEAWEEDFS_NAME="exocomp-e2e-seaweedfs-$$"
SEAWEEDFS_USER="exocomp-e2e-user"
SEAWEEDFS_PASS="exocomp-e2e-pass-$$"
SEAWEEDFS_CONFIG_DIR="$WORK/seaweedfs-config"
mkdir -p "$SEAWEEDFS_CONFIG_DIR"
# world-readable, not 0600: rootless podman's user-namespace UID remap means
# the container's own user is neither this file's nor its directory's owner
# (verified against a real run). It holds only this throwaway fixture's
# dummy credential, not a secret.
cat >"$SEAWEEDFS_CONFIG_DIR/s3.json" <<EOF
{
  "identities": [
    {
      "name": "$SEAWEEDFS_USER",
      "credentials": [{"accessKey": "$SEAWEEDFS_USER", "secretKey": "$SEAWEEDFS_PASS"}],
      "actions": ["Admin", "Read", "Write"]
    }
  ]
}
EOF
chmod 0755 "$SEAWEEDFS_CONFIG_DIR"
chmod 0644 "$SEAWEEDFS_CONFIG_DIR/s3.json"

podman run -d --rm --name "$SEAWEEDFS_NAME" \
    --publish 127.0.0.1::8333 \
    --volume "$SEAWEEDFS_CONFIG_DIR:/etc/seaweedfs:Z" \
    docker.io/chrislusf/seaweedfs:latest server -s3 -s3.port=8333 \
    -s3.config=/etc/seaweedfs/s3.json -dir=/data >"$WORK/seaweedfs.log" 2>&1 \
    || { cat "$WORK/seaweedfs.log"; fail "podman run seaweedfs failed"; }

SEAWEEDFS_HOSTPORT=$(podman port "$SEAWEEDFS_NAME" 8333/tcp)
[ -n "$SEAWEEDFS_HOSTPORT" ] || fail "could not read seaweedfs's published port"
SEAWEEDFS_ENDPOINT="http://$SEAWEEDFS_HOSTPORT"

# No -f: SeaweedFS answers an unauthenticated GET with 403, not 200 (verified
# against a real run) — readiness here means the S3 API answers HTTP at all,
# not any particular status.
i=0
until curl -sS -o /dev/null "$SEAWEEDFS_ENDPOINT/" 2>/dev/null; do
    i=$((i + 1))
    if [ "$i" -ge 360 ]; then
        podman logs --tail 20 "$SEAWEEDFS_NAME"
        fail "seaweedfs did not become healthy within 90s"
    fi
    sleep 0.25
done

export AWS_ACCESS_KEY_ID="$SEAWEEDFS_USER"
export AWS_SECRET_ACCESS_KEY="$SEAWEEDFS_PASS"
export AWS_DEFAULT_REGION="us-east-1"

log "three-leg backup (restic_local, restic_s3, git)"
git init -q --bare "$WORK/git-target.git"
"$EXO" backup add-target --kind restic_local --name nas --uri "$WORK/restic-repo"
"$EXO" backup add-target --kind restic_s3 --name offsite --uri "s3:$SEAWEEDFS_ENDPOINT/exocomp-e2e-backup"
"$EXO" backup add-target --kind git --name gh --uri "$WORK/git-target.git"
"$EXO" backup run >"$WORK/backup.out" 2>&1 || { cat "$WORK/backup.out"; fail "backup run failed"; }
[ -f "$WORK/restic-repo/keyring.age" ] || fail "keyring.age missing at restic_local target root"
git -C "$WORK/git-target.git" rev-list --count refs/heads/backup | grep -qx 1 \
    || fail "git leg is not a single rolling orphan commit"

log "restore from every leg with the RECOVERY identity, canary decrypts on each"
"$EXO" restore --from "restic_local:$WORK/restic-repo" --dest "$WORK/restored-nas" \
    --identity "$WORK/recovery.key" --target-name nas --canary canary \
    >"$WORK/restore-nas.out" 2>&1 || { cat "$WORK/restore-nas.out"; fail "restic_local restore failed"; }
grep -q "canary: canary opened" "$WORK/restore-nas.out" \
    || fail "canary did not decrypt on the restic_local restore"
age -d -i "$WORK/recovery.key" "$WORK/restored-nas/keyring.age" | wc -c | grep -qx 32 \
    || fail "restic_local restored keyring does not unwrap to a 32-byte DEK"

"$EXO" restore --from "restic_s3:s3:$SEAWEEDFS_ENDPOINT/exocomp-e2e-backup" --dest "$WORK/restored-s3" \
    --identity "$WORK/recovery.key" --target-name offsite --canary canary \
    >"$WORK/restore-s3.out" 2>&1 || { cat "$WORK/restore-s3.out"; fail "restic_s3 restore failed"; }
grep -q "canary: canary opened" "$WORK/restore-s3.out" \
    || fail "canary did not decrypt on the restic_s3 restore"

"$EXO" restore --from "git:$WORK/git-target.git" --dest "$WORK/restored-git" \
    --identity "$WORK/recovery.key" --canary canary \
    >"$WORK/restore-git.out" 2>&1 || { cat "$WORK/restore-git.out"; fail "git restore failed"; }
grep -q "canary: canary opened" "$WORK/restore-git.out" \
    || fail "canary did not decrypt on the git restore"

log "stop node"
limactl stop "$INSTANCE" >/dev/null 2>&1 || true

printf 'PASS: m1 gate — vaulted play on %s, redacted events, three-leg backup (restic_local, restic_s3, git), recovery-identity restore + canary decrypt on every leg\n' "$NODE"
