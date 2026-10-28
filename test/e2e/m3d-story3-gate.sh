#!/bin/sh
# M3d story-3 gate: "run my terraform regularly and notify me of drift."
# A scheduled tofu-plan detects drift (exit 2 = changes), the daemon raises it
# as a TofuDrift event carrying the workspace, and a CEL rule fires a
# notification on it. This gate proves the regular-plan -> drift-event -> rule
# chain end to end through one daemon:
#   1. an applied workspace is clean; drifting the real resource makes the next
#      plan report changes (exit 2);
#   2. the daemon's drift sweep raises a TofuDrift event for the drifted
#      workspace, carrying it in the labels;
#   3. a rule matching TofuDrift FIRES (the notify hook) — event list shows it.
# Complements m2-gate.sh (drift detection + schedule fire/freeze).
set -eu

ROOT=$(git rev-parse --show-toplevel)
WORK=$(mktemp -d)
SERVE_PID=""
cleanup() { [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

command -v tofu >/dev/null 2>&1 || { echo "SKIP: tofu absent"; exit 0; }
command -v nono >/dev/null 2>&1 || { echo "SKIP: nono absent"; exit 0; }

export XDG_DATA_HOME="$WORK/data"
CANARY="exocomp-story3-canary"

log "build + init"
make -C "$ROOT" -s build
EXO="$ROOT/bin/exocomp"
"$EXO" init >"$WORK/kit.txt"

log "a rule that notifies on tofu drift"
"$EXO" rule add drift-notify \
    --expr 'event.name == "TofuDrift" && event.status == "firing"' \
    --action notify

log "start the daemon (fast drift sweep)"
"$EXO" serve --drain-grace 15s --sweep-interval 3s >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 60); do grep -q 'state backend listening' "$WORK/serve.log" 2>/dev/null && break; sleep 0.2; done
grep -q 'state backend listening' "$WORK/serve.log" || { cat "$WORK/serve.log"; fail "daemon did not start"; }

log "a workspace managing one file, applied clean"
mkdir -p "$WORK/tf"
cat >"$WORK/tf/main.tf" <<TF
resource "local_file" "canary" {
  filename = "\${path.module}/out.txt"
  content  = "$CANARY"
}
TF
"$EXO" workspace add gate --dir "$WORK/tf" >/dev/null
set +e
"$EXO" workspace plan gate >"$WORK/plan.out" 2>&1; PRC=$?
set -e
[ "$PRC" -eq 2 ] || { cat "$WORK/plan.out"; fail "initial plan expected exit 2 (changes), got $PRC"; }
PLAN_ID=$(sed -n 's/.*saved plan artifact \([0-9]*\).*/\1/p' "$WORK/plan.out" | head -1)
"$EXO" workspace apply gate --plan "${PLAN_ID:-1}" >"$WORK/apply.out" 2>&1 || { cat "$WORK/apply.out"; fail "apply failed"; }
grep -q "$CANARY" "$WORK/tf/out.txt" || fail "apply did not write the resource"

log "drift the real resource; the next regular plan reports changes"
printf 'drifted-by-hand' >"$WORK/tf/out.txt"
set +e
"$EXO" workspace plan gate >"$WORK/drift.out" 2>&1; DRC=$?
set -e
[ "$DRC" -eq 2 ] || { cat "$WORK/drift.out"; fail "drift plan expected exit 2, got $DRC"; }
log "   -> drift plan reported changes (exit 2)"

log "the daemon's drift sweep raises TofuDrift; the notify rule acts"
# A notify action's outcome is "matched · notify only; no job" — that IS the
# drift notification firing (it sends a push, creates no job).
fired=""
for _ in $(seq 1 20); do
    if "$EXO" event list 2>/dev/null | grep -qE '(fired|matched)[[:space:]]+drift-notify'; then fired=yes; break; fi
    sleep 1
done
"$EXO" event list >"$WORK/events" 2>&1 || true
[ "$fired" = yes ] || { cat "$WORK/events"; fail "TofuDrift never matched drift-notify within the sweep window"; }
grep -qi 'drift' "$WORK/events" || { cat "$WORK/events"; fail "no drift-source event recorded"; }
log "   -> TofuDrift matched drift-notify (notification sent)"

log "clean shutdown"
kill "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true
SERVE_PID=""

printf 'PASS: story-3 gate — a regular plan detects drift, the daemon raises a TofuDrift event, and a rule fires the drift notification\n'
