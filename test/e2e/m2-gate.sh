#!/bin/sh
# M2 gate: one serve daemon runs the state backend, scheduler, and queue;
# CLI enqueues and tails; encrypted tofu state + drift; vaulted ansible via
# the daemon; schedules fire and freeze on disable; SSH TUI answers; clean
# shutdown leaves no non-terminal rows. Complements m1-gate.sh (node exec).
set -eu

ROOT=$(git rev-parse --show-toplevel)
WORK=$(mktemp -d)
SERVE_PID=""
SSH_PID=""
cleanup() {
    [ -n "$SSH_PID" ] && kill "$SSH_PID" 2>/dev/null || true
    [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export PATH="$HOME/.local/bin:$PATH"
export XDG_DATA_HOME="$WORK/data"
# Hermetic client env: a hosts.toml default remote, an inherited sandbox
# tier, or an inherited output format would all change what these commands do.
export XDG_CONFIG_HOME="$WORK/config"
export EXOCOMP_REMOTE=
export EXOCOMP_SANDBOX=
export EXOCOMP_OUTPUT=
DATADIR="$XDG_DATA_HOME/exocomp"
DB="$DATADIR/exocomp.db"

# Self-skip on a host without the tools this gate drives (the daemon runs jobs
# at the enforcing sandbox tier, so nono is required), so a hosted run reports
# SKIP rather than a failure (see .github/workflows/e2e.yml).
for tool in nono tofu ansible-playbook ansible-vault age openssl ssh-keygen ssh python3 curl; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'SKIP: %s absent\n' "$tool"; exit 0; }
done

log "build + init"
make -C "$ROOT" -s build
EXO="$ROOT/bin/exocomp"
"$EXO" init >"$WORK/kit.txt"

log "start the daemon"
"$EXO" serve --drain-grace 15s >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 50); do
    grep -q 'state backend listening' "$WORK/serve.log" 2>/dev/null && break
    sleep 0.2
done
ADDR=$(sed -n 's/.*state backend listening on //p' "$WORK/serve.log" | head -1)
[ -n "$ADDR" ] || { cat "$WORK/serve.log"; fail "daemon did not start"; }
curl -fsS "$ADDR/healthz" >/dev/null || fail "healthz not answering"

log "second daemon refused"
if "$EXO" serve >"$WORK/serve2.log" 2>&1; then fail "second serve did not refuse"; fi
grep -q 'already owns' "$WORK/serve2.log" || fail "refusal did not name the holder"

log "workspace plan/apply through the daemon (encrypted state)"
mkdir -p "$WORK/tf"
CANARY="m2-state-canary-$$"
cat >"$WORK/tf/main.tf" <<EOF
resource "local_file" "gate" {
  filename = "\${path.module}/gate.txt"
  content  = "$CANARY"
}
EOF
"$EXO" workspace add gate --dir "$WORK/tf" >/dev/null
set +e
"$EXO" workspace plan gate >"$WORK/plan.out" 2>&1; PLAN_RC=$?
set -e
[ "$PLAN_RC" -eq 2 ] || { cat "$WORK/plan.out"; fail "plan expected exit 2 (changes), got $PLAN_RC"; }
grep -q 'queued for the running daemon' "$WORK/plan.out" || fail "plan did not take the daemon path"
PLAN_ID=$(sed -n 's/.*saved plan artifact \([0-9]*\).*/\1/p' "$WORK/plan.out" | head -1)
"$EXO" workspace apply gate --plan "${PLAN_ID:-1}" >"$WORK/apply.out" 2>&1 || { cat "$WORK/apply.out"; fail "apply failed"; }
grep -q "$CANARY" "$WORK/tf/gate.txt" || fail "apply did not write the resource"
grep -aq "$CANARY" "$DB" && fail "state canary appears unencrypted in the database" || true
printf 'drifted' >"$WORK/tf/gate.txt"
set +e
"$EXO" workspace plan gate >"$WORK/drift.out" 2>&1; DRIFT_RC=$?
set -e
[ "$DRIFT_RC" -eq 2 ] || fail "drift plan expected exit 2, got $DRIFT_RC"

log "vaulted playbook via the daemon (redaction)"
DEK_HEX=$(grep -o 'AGE-SECRET-KEY-1[A-Z0-9]*' "$WORK/kit.txt" | head -1 >"$WORK/rk"; age -d -i "$WORK/rk" "$DATADIR/keyring.age" | od -An -tx1 | tr -d ' \n')
openssl kdf -keylen 32 -kdfopt digest:SHA256 -kdfopt hexkey:"$DEK_HEX" \
    -kdfopt info:ansible-vault:m2 -kdfopt salt:exocomp/v1 -binary HKDF | base64 -w0 >"$WORK/vp"
MARK="m2-vault-mark-$$"
# Redaction covers envelope-derived material and SEALED secrets; seal the
# marker so the redactor knows it (the documented boundary since M1).
printf '%s' "$MARK" | "$EXO" secrets add m2-canary --kind e2e >/dev/null
# Playbook sources live under $WORK/src, not $WORK itself: a sandboxed job
# reads the playbook's directory, and $WORK also holds the data dir ($WORK/data)
# — a source path may not be an ancestor of the data dir, so it gets its own.
SRC="$WORK/src"
mkdir -p "$SRC"
printf '%s' "$MARK" | ansible-vault encrypt_string --vault-id exocomp@"$WORK/vp" --stdin-name marker >"$SRC/vaulted.yml"
cat >"$SRC/play.yml" <<EOF
- hosts: localhost
  connection: local
  gather_facts: false
  vars_files: [$SRC/vaulted.yml]
  tasks:
    - ansible.builtin.debug:
        msg: "marker is {{ marker }}"
EOF
"$EXO" playbook run "$SRC/play.yml" --vault-project m2 >"$WORK/play.out" 2>&1 || { cat "$WORK/play.out"; fail "playbook failed"; }
grep -q 'queued for the running daemon' "$WORK/play.out" || fail "playbook did not take the daemon path"
grep -q 'REDACTED' "$WORK/play.out" || fail "no redaction marker — decryption may not have happened"
grep -aq -- "$MARK" "$DB" && fail "vaulted value leaked into the database" || true

log "schedules fire, then freeze on disable"
"$EXO" schedule add tick --cron '@every 1s' --kind shell -- /bin/sh -c "date >> $WORK/ticks" >/dev/null
kill -HUP "$SERVE_PID"
sleep 4
TICKS1=$(wc -l < "$WORK/ticks" 2>/dev/null || echo 0)
[ "$TICKS1" -ge 2 ] || fail "schedule fired $TICKS1 times in 4s, want >=2"
"$EXO" schedule disable tick >/dev/null
kill -HUP "$SERVE_PID"
sleep 1; TICKS2=$(wc -l < "$WORK/ticks"); sleep 3
TICKS3=$(wc -l < "$WORK/ticks")
[ "$TICKS3" -le $((TICKS2 + 1)) ] || fail "disabled schedule kept firing ($TICKS2 -> $TICKS3)"

log "ssh TUI probe"
ssh-keygen -q -t ed25519 -N '' -f "$WORK/opkey"
SSH_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
"$EXO" ssh-serve --listen 127.0.0.1:"$SSH_PORT" --authorized-keys "$WORK/opkey.pub" >"$WORK/ssh.log" 2>&1 &
SSH_PID=$!
sleep 1.5
python3 - "$SSH_PORT" "$WORK/opkey" >"$WORK/tui.out" 2>/dev/null <<'PYEOF' || true
import os, pty, select, struct, sys, termios, time, fcntl
port, key = sys.argv[1], sys.argv[2]
pid, fd = pty.fork()
if pid == 0:
    os.execvp("ssh", ["ssh", "-tt", "-p", port, "-i", key,
                      "-o", "StrictHostKeyChecking=no",
                      "-o", "UserKnownHostsFile=/dev/null",
                      "-o", "IdentitiesOnly=yes", "127.0.0.1"])
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
out, sent, deadline = b"", False, time.time() + 15
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 0.25)
    if r:
        try:
            chunk = os.read(fd, 4096)
        except OSError:
            break
        if not chunk:
            break
        out += chunk
    if not sent and time.time() > deadline - 12:
        time.sleep(2); os.write(fd, b"q"); sent = True
os.close(fd)
sys.stdout.buffer.write(out)
PYEOF
grep -aq 'exocomp' "$WORK/tui.out" || fail "SSH session did not render the TUI"

log "clean shutdown"
kill -TERM "$SERVE_PID"
wait "$SERVE_PID" 2>/dev/null; SERVE_RC=$?
SERVE_PID=""
[ "$SERVE_RC" -eq 0 ] || fail "serve exited $SERVE_RC"
LEFT=$(python3 - "$DB" <<'EOF'
import sqlite3,sys
c=sqlite3.connect(sys.argv[1])
print(c.execute("select count(*) from job where state in ('queued','running')").fetchone()[0])
EOF
)
[ "$LEFT" = "0" ] || fail "$LEFT non-terminal job rows after shutdown"

printf 'PASS: m2 gate — daemon-run tofu (encrypted state + drift) and vaulted ansible, schedule fire/freeze, single-instance lock, SSH TUI, clean shutdown\n'
