#!/bin/sh
# M1 gate: init -> vaulted playbook against a lima node (redacted events) ->
# three-leg backup (restic_local, restic_s3 via MinIO-under-podman, git) ->
# restore from every leg with the RECOVERY identity, canary decrypts on each.
# Run from the repo root. Requires: lima node template under test/lima/,
# ansible-playbook, restic, age, openssl, git, podman, curl. Never uses sudo.
set -eu

ROOT=$(git rev-parse --show-toplevel)
INSTANCE=${EXOCOMP_E2E_INSTANCE:-exocomp-node0}
WORK=$(mktemp -d)
MINIO_NAME=""

cleanup() {
    if [ -n "$MINIO_NAME" ]; then
        podman rm -f --time 1 "$MINIO_NAME" >/dev/null 2>&1 || true
    fi
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
# podman must keep the host's own storage location: it defaults rootless
# storage under XDG_DATA_HOME, and that is about to be redirected into WORK
# for exocomp's data dir. Sharing it would put subuid-owned container layers
# under WORK, which a plain `rm -rf` at cleanup cannot remove.
podman() { env -u XDG_DATA_HOME command podman "$@"; }

export PATH="$HOME/.local/bin:$PATH"
export XDG_DATA_HOME="$WORK/data"
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
limactl show-ssh --format=config "$INSTANCE" >"$WORK/ssh_config"
NODE="lima-$INSTANCE"
cat >"$WORK/inventory.ini" <<EOF
[nodes]
$NODE
[nodes:vars]
ansible_ssh_common_args=-F $WORK/ssh_config
EOF

log "vaulted playbook against the node (canary value as the vaulted var)"
printf '%s' "$CANARY_VALUE" | ansible-vault encrypt_string \
    --vault-id exocomp@"$WORK/vault-pass" --stdin-name marker >"$WORK/vaulted-var.yml"
cat >"$WORK/play.yml" <<EOF
- hosts: nodes
  gather_facts: false
  vars_files:
    - $WORK/vaulted-var.yml
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
"$EXO" playbook run "$WORK/play.yml" --inventory "$WORK/inventory.ini" \
    --vault-project e2e >"$WORK/play.out" 2>&1 || { cat "$WORK/play.out"; fail "playbook run failed"; }
limactl shell "$INSTANCE" -- cat /tmp/exocomp-e2e | grep -q "$CANARY_VALUE" \
    || fail "marker not written on node"

log "redaction: plaintext absent from the database, [REDACTED] present in events"
if grep -aq -- "$CANARY_VALUE" "$DATADIR/exocomp.db"; then
    fail "vaulted value leaked into exocomp.db"
fi
grep -q '\[REDACTED\]' "$WORK/play.out" \
    || fail "redaction marker absent from the job's captured events"

log "start MinIO under podman for the restic_s3 leg"
MINIO_NAME="exocomp-e2e-minio-$$"
MINIO_USER="exocomp-e2e-user"
MINIO_PASS="exocomp-e2e-pass-$$"
podman run -d --rm --name "$MINIO_NAME" \
    --publish 127.0.0.1::9000 \
    --env MINIO_ROOT_USER="$MINIO_USER" \
    --env MINIO_ROOT_PASSWORD="$MINIO_PASS" \
    quay.io/minio/minio:latest server /data >"$WORK/minio.log" 2>&1 \
    || { cat "$WORK/minio.log"; fail "podman run minio failed"; }

MINIO_HOSTPORT=$(podman port "$MINIO_NAME" 9000/tcp)
[ -n "$MINIO_HOSTPORT" ] || fail "could not read minio's published port"
MINIO_ENDPOINT="http://$MINIO_HOSTPORT"

i=0
until curl -fsS "$MINIO_ENDPOINT/minio/health/live" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge 360 ]; then
        podman logs --tail 20 "$MINIO_NAME"
        fail "minio did not become healthy within 90s"
    fi
    sleep 0.25
done

export AWS_ACCESS_KEY_ID="$MINIO_USER"
export AWS_SECRET_ACCESS_KEY="$MINIO_PASS"
export AWS_DEFAULT_REGION="us-east-1"

log "three-leg backup (restic_local, restic_s3, git)"
git init -q --bare "$WORK/git-target.git"
"$EXO" backup add-target --kind restic_local --name nas --uri "$WORK/restic-repo"
"$EXO" backup add-target --kind restic_s3 --name offsite --uri "s3:$MINIO_ENDPOINT/exocomp-e2e-backup"
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

"$EXO" restore --from "restic_s3:s3:$MINIO_ENDPOINT/exocomp-e2e-backup" --dest "$WORK/restored-s3" \
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
