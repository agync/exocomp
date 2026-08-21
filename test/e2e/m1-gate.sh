#!/bin/sh
# M1 gate: init -> vaulted playbook against a lima node (redacted events) ->
# three-leg backup -> restore with the RECOVERY identity -> canary present.
# Run from the repo root. Requires: lima node template under test/lima/,
# ansible-playbook, restic, age, openssl, git. Never uses sudo.
set -eu

ROOT=$(git rev-parse --show-toplevel)
INSTANCE=${EXOCOMP_E2E_INSTANCE:-exocomp-node0}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

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
printf 'e2e-canary-%s' "$$" | "$EXO" secrets add canary --kind e2e
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

log "vaulted playbook against the node"
SECRET_MARK="vaulted-mark-$$"
printf '%s' "$SECRET_MARK" | ansible-vault encrypt_string \
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
EOF
"$EXO" playbook run "$WORK/play.yml" --inventory "$WORK/inventory.ini" \
    --vault-project e2e >"$WORK/play.out" 2>&1 || { cat "$WORK/play.out"; fail "playbook run failed"; }
limactl shell "$INSTANCE" -- cat /tmp/exocomp-e2e | grep -q "$SECRET_MARK" \
    || fail "marker not written on node"

log "redaction: vaulted value absent from the database"
if grep -aq -- "$SECRET_MARK" "$DATADIR/exocomp.db"; then
    fail "vaulted value leaked into exocomp.db"
fi

log "three-leg backup (local restic, bare git; s3 optional)"
git init -q --bare "$WORK/git-target.git"
"$EXO" backup add-target --kind restic_local --name nas --uri "$WORK/restic-repo"
"$EXO" backup add-target --kind git --name gh --uri "$WORK/git-target.git"
"$EXO" backup run >"$WORK/backup.out" 2>&1 || { cat "$WORK/backup.out"; fail "backup run failed"; }
[ -f "$WORK/restic-repo/keyring.age" ] || fail "keyring.age missing at restic target root"
git -C "$WORK/git-target.git" rev-list --count refs/heads/backup | grep -qx 1 \
    || fail "git leg is not a single rolling orphan commit"

log "restore with the RECOVERY identity into a fresh dir"
"$EXO" restore --from "restic_local:$WORK/restic-repo" --dest "$WORK/restored" \
    --identity "$WORK/recovery.key" --target-name nas >"$WORK/restore.out" 2>&1 \
    || { cat "$WORK/restore.out"; fail "restore failed"; }
age -d -i "$WORK/recovery.key" "$WORK/restored/keyring.age" | wc -c | grep -qx 32 \
    || fail "restored keyring does not unwrap to a 32-byte DEK"
XDG_DATA_HOME="$WORK/restored-xdg" sh -c '
    mkdir -p "$XDG_DATA_HOME/exocomp"
    cp "$1/restored/snapshot.db" "$XDG_DATA_HOME/exocomp/exocomp.db" 2>/dev/null ||
    cp "$1/restored/exocomp.db" "$XDG_DATA_HOME/exocomp/exocomp.db"
    exec "$2" secrets list' -- "$WORK" "$EXO" | grep -q canary \
    || fail "canary secret missing from restored database"

log "stop node"
limactl stop "$INSTANCE" >/dev/null 2>&1 || true

printf 'PASS: m1 gate — vaulted play on %s, redacted events, 2-leg backup, recovery-identity restore, canary intact\n' "$NODE"
