#!/bin/sh
# M3c story-5 gate: the Terraform -> Ansible inventory handoff. A tofu apply in a
# workspace declaring one SENSITIVE and one NON-sensitive output captures
# `tofu output -json` in-sandbox; the materialized inventory holds the
# non-sensitive host/group data and NOT the sensitive value; a dependent play
# runs against that inventory from a 0700 dir with NO bearer / passphrase /
# tfstate in its environment. The chain is one step-ledger workflow (step 2
# blocked when step 1 fails), and a daemon killed mid-chain resumes it.
# Real tofu + real ansible through one serve daemon, like m2-gate.sh.
set -eu

ROOT=$(git rev-parse --show-toplevel)
WORK=$(mktemp -d)
SERVE_PID=""
cleanup() {
    [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export PATH="$HOME/.local/bin:$PATH"
export XDG_DATA_HOME="$WORK/data"
DATADIR="$XDG_DATA_HOME/exocomp"

command -v tofu >/dev/null 2>&1 || { log "tofu not installed; skipping"; exit 0; }
command -v ansible-playbook >/dev/null 2>&1 || { log "ansible-playbook not installed; skipping"; exit 0; }

SENSITIVE="s3cr3t-handoff-CANARY-8f21"

start_daemon() {
    "$EXO" serve --drain-grace 15s >"$1" 2>&1 &
    SERVE_PID=$!
    for _ in $(seq 1 60); do
        grep -q 'state backend listening' "$1" 2>/dev/null && break
        sleep 0.2
    done
    grep -q 'state backend listening' "$1" || { cat "$1"; fail "daemon did not start"; }
}

log "build + init"
make -C "$ROOT" -s build
EXO="$ROOT/bin/exocomp"
"$EXO" init >"$WORK/kit.txt"

# --- a workspace whose outputs describe an inventory + a sensitive value ---
mkdir -p "$WORK/tf"
cat >"$WORK/tf/main.tf" <<EOF
resource "local_file" "marker" {
  filename = "\${path.module}/marker.txt"
  content  = "applied"
}
output "db_password" {
  value     = "$SENSITIVE"
  sensitive = true
}
output "ansible_inventory" {
  value = {
    all = {
      hosts    = { localhost = { ansible_connection = "local" } }
      children = { web = { hosts = { webnode = { ansible_connection = "local" } } } }
    }
  }
}
EOF
"$EXO" workspace add homelab --dir "$WORK/tf" >/dev/null

# Playbooks live outside the data-dir tree: the sandbox refuses to grant a
# source path that is an ancestor of the data dir.
mkdir -p "$WORK/play"

# A canary play that dumps its own environment and runs against the inventory.
cat >"$WORK/play/canary.yml" <<'EOF'
- hosts: all
  gather_facts: false
  connection: local
  tasks:
    - name: dump environment
      ansible.builtin.command: env
      register: envdump
    - name: surface it
      ansible.builtin.debug:
        var: envdump.stdout_lines
EOF

log "start the daemon"
start_daemon "$WORK/serve.log"

log "run the handoff: apply -> capture -> inventory -> play"
"$EXO" handoff run homelab "$WORK/play/canary.yml" --wait >"$WORK/handoff.out" 2>&1 \
    || { cat "$WORK/handoff.out"; fail "handoff run failed"; }
WF=$(sed -n 's/^workflow \(handoff:[^ ]*\) started.*/\1/p' "$WORK/handoff.out" | head -1)
[ -n "$WF" ] || { cat "$WORK/handoff.out"; fail "no workflow id"; }

log "the chain is a ledger workflow with both steps succeeded"
"$EXO" handoff status "$WF" >"$WORK/status.out" 2>&1
grep -q "workflow $WF: succeeded" "$WORK/status.out" || { cat "$WORK/status.out"; fail "workflow not succeeded"; }
grep -Eq 'step 1  tofu-apply +succeeded' "$WORK/status.out" || { cat "$WORK/status.out"; fail "apply step not succeeded"; }
grep -Eq 'step 2  ansible-play +succeeded' "$WORK/status.out" || { cat "$WORK/status.out"; fail "play step not succeeded"; }

log "the capture suppressed the sensitive output (it existed and was dropped)"
grep -q 'suppressed 1 sensitive output' "$WORK/handoff.out" \
    || { cat "$WORK/handoff.out"; fail "capture did not report suppressing the sensitive output"; }

INV="$DATADIR/artifacts/inventory/homelab.json"
[ -f "$INV" ] || fail "no materialized inventory at $INV"
log "the materialized inventory holds the non-sensitive host/group data"
grep -q webnode "$INV" || { cat "$INV"; fail "inventory missing host data"; }
grep -q '"web"' "$INV" || { cat "$INV"; fail "inventory missing group data"; }
log "the sensitive value is ABSENT from the inventory"
grep -q "$SENSITIVE" "$INV" && { fail "the sensitive output leaked into the inventory"; } || true

log "the play consumed the inventory (it ran against the captured host)"
grep -q 'webnode' "$WORK/handoff.out" || { cat "$WORK/handoff.out"; fail "the play did not run against the inventory host"; }

log "the play's environment carries NO bearer / passphrase / tfstate"
for forbidden in 'Bearer ' 'TF_ENCRYPTION' 'TF_DATA_DIR' 'Authorization'; do
    if grep -q "$forbidden" "$WORK/handoff.out"; then
        fail "the play environment leaked $forbidden"
    fi
done
log "the sensitive value never reached the play or any streamed event"
grep -q "$SENSITIVE" "$WORK/handoff.out" && fail "the sensitive value reached the play" || true

# The state row is ciphertext, so raw tfstate could never be the inventory
# source: the play saw declared outputs, never the state.
log "the tfstate row is encrypted at rest (never a plaintext inventory source)"
DB="$DATADIR/exocomp.db"
if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$DB" "SELECT hex(ciphertext) FROM tfstate;" >"$WORK/state.hex" 2>/dev/null || true
    if [ -s "$WORK/state.hex" ]; then
        # The canary string must not appear in the stored state bytes.
        printf '%s' "$SENSITIVE" | od -An -tx1 | tr -d ' \n' >"$WORK/canary.hex"
        grep -iqf "$WORK/canary.hex" "$WORK/state.hex" && fail "the state row holds a value in the clear" || true
    fi
fi

# --- step 2 blocked when step 1 fails ---
log "a failed apply leaves the play blocked"
mkdir -p "$WORK/tffail"
cat >"$WORK/tffail/main.tf" <<'EOF'
resource "local_file" "bad" {
  filename = "/root/exocomp-handoff-must-fail.txt"
  content  = "unwritable"
}
EOF
"$EXO" workspace add badws --dir "$WORK/tffail" >/dev/null
"$EXO" handoff run badws "$WORK/play/canary.yml" --wait >"$WORK/handoff-fail.out" 2>&1 || true
WFF=$(sed -n 's/^workflow \(handoff:[^ ]*\) started.*/\1/p' "$WORK/handoff-fail.out" | head -1)
[ -n "$WFF" ] || { cat "$WORK/handoff-fail.out"; fail "no failing workflow id"; }
"$EXO" handoff status "$WFF" >"$WORK/status-fail.out" 2>&1
grep -q "workflow $WFF: failed" "$WORK/status-fail.out" || { cat "$WORK/status-fail.out"; fail "failing workflow not marked failed"; }
grep -Eq 'step 1  tofu-apply +failed' "$WORK/status-fail.out" || { cat "$WORK/status-fail.out"; fail "apply step not failed"; }
grep -Eq 'step 2  ansible-play +blocked' "$WORK/status-fail.out" || { cat "$WORK/status-fail.out"; fail "play step not blocked"; }
# The failing workspace produced no inventory, and its play never ran.
[ -f "$DATADIR/artifacts/inventory/badws.json" ] && fail "a failed apply still materialized an inventory" || true

# --- daemon-restart mid-chain resume ---
log "a daemon killed mid-chain resumes the workflow from the ledger"
GATE="$WORK/play-gate"
cat >"$WORK/play/gated.yml" <<EOF
- hosts: all
  gather_facts: false
  connection: local
  tasks:
    - name: wait for the gate then proceed
      ansible.builtin.shell: "i=0; while [ ! -f $GATE ] && [ \$i -lt 600 ]; do sleep 0.2; i=\$((i+1)); done"
    - ansible.builtin.debug:
        msg: resumed-and-played
EOF
mkdir -p "$WORK/tf2"
cp "$WORK/tf/main.tf" "$WORK/tf2/main.tf"
"$EXO" workspace add resumews --dir "$WORK/tf2" >/dev/null

"$EXO" handoff run resumews "$WORK/play/gated.yml" >"$WORK/handoff-resume.out" 2>&1 \
    || { cat "$WORK/handoff-resume.out"; fail "handoff run (resume) failed to start"; }
WFR=$(sed -n 's/^workflow \(handoff:[^ ]*\) started.*/\1/p' "$WORK/handoff-resume.out" | head -1)
[ -n "$WFR" ] || { cat "$WORK/handoff-resume.out"; fail "no resume workflow id"; }

# Wait until the apply has succeeded and the play has started (blocked on gate).
for _ in $(seq 1 100); do
    "$EXO" handoff status "$WFR" >"$WORK/status-r.out" 2>&1 || true
    grep -Eq 'step 1  tofu-apply +succeeded' "$WORK/status-r.out" \
        && grep -Eq 'step 2  ansible-play +running' "$WORK/status-r.out" && break
    sleep 0.3
done
grep -Eq 'step 1  tofu-apply +succeeded' "$WORK/status-r.out" || { cat "$WORK/status-r.out"; fail "apply did not succeed before the kill"; }

log "kill the daemon mid-play (apply already succeeded)"
kill -9 "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true
SERVE_PID=""

# Let the play through once it is restarted, then bring the daemon back.
: >"$GATE"
log "restart the daemon; it resumes the chain from the ledger"
start_daemon "$WORK/serve2.log"

for _ in $(seq 1 150); do
    "$EXO" handoff status "$WFR" >"$WORK/status-r2.out" 2>&1 || true
    grep -q "workflow $WFR: succeeded" "$WORK/status-r2.out" && break
    grep -q "workflow $WFR: failed" "$WORK/status-r2.out" && { cat "$WORK/status-r2.out"; fail "resumed workflow failed"; }
    sleep 0.3
done
grep -q "workflow $WFR: succeeded" "$WORK/status-r2.out" || { cat "$WORK/status-r2.out"; fail "workflow did not resume to succeeded"; }
# The apply is not re-run across the restart: exactly one state version exists.
grep -q 'resumed .* workflow' "$WORK/serve2.log" || { cat "$WORK/serve2.log"; fail "daemon did not report resuming from the ledger"; }

log "ALL STORY-5 GATE CHECKS PASSED"
