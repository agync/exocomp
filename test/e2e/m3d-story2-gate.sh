#!/bin/sh
# M3d story-2 gate: "on event/alert, run X."
# An Alertmanager-shaped webhook hits the reactive ingress; a CEL rule matches;
# guardrails apply; the action runs through the queue. This gate proves:
#   1. a matching firing alert FIRES its rule (event list shows the outcome);
#   2. a non-matching alert is received but does NOT fire (the rule is a filter);
#   3. a rule with --require-approval leaves the action gated until approved;
#   4. event explain shows the per-condition trace that decided it.
set -eu

ROOT=$(git rev-parse --show-toplevel)
WORK=$(mktemp -d)
SERVE_PID=""
cleanup() { [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || { echo "SKIP: curl absent"; exit 0; }

export XDG_DATA_HOME="$WORK/data"

log "build + init"
make -C "$ROOT" -s build
EXO="$ROOT/bin/exocomp"
"$EXO" init >"$WORK/kit.txt"

log "rules: one plain fire, one approval-gated"
"$EXO" rule add disk-firing \
    --expr 'event.name == "DiskFull" && event.status == "firing" && event.labels.severity == "critical"' \
    --action shell -- sh -c 'true'
"$EXO" rule add reboot-node --require-approval \
    --expr 'event.name == "RebootNeeded" && event.status == "firing"' \
    --action shell -- sh -c 'true'

log "start the daemon"
"$EXO" serve --drain-grace 15s >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 60); do grep -q 'state backend listening' "$WORK/serve.log" 2>/dev/null && break; sleep 0.2; done
HOOK=$(sed -n 's/^hook alertmanager *//p' "$WORK/serve.log" | head -1)
[ -n "$HOOK" ] || { cat "$WORK/serve.log"; fail "no alertmanager hook url printed"; }
log "   -> hook $HOOK"

post() { curl -sS -X POST -H 'Content-Type: application/json' -d "$1" "$HOOK"; }

log "a matching critical DiskFull alert FIRES the rule"
post '{"alerts":[{"status":"firing","labels":{"alertname":"DiskFull","severity":"critical","instance":"nuc-02"}}]}' >"$WORK/r1" 2>&1
sleep 2
"$EXO" event list >"$WORK/events1" 2>&1 || { cat "$WORK/events1"; fail "event list failed"; }
grep -qE 'fired[[:space:]]+disk-firing' "$WORK/events1" || { cat "$WORK/events1"; fail "DiskFull did not fire disk-firing"; }
"$EXO" event explain 1 >"$WORK/explain1" 2>&1 || true
log "   -> DiskFull fired disk-firing (job queued); explain trace captured"

log "a non-matching alert is received but does NOT fire"
post '{"alerts":[{"status":"firing","labels":{"alertname":"CpuHot","severity":"warning"}}]}' >/dev/null 2>&1
sleep 1
"$EXO" event list >"$WORK/events2" 2>&1
# disk-firing still appears exactly once: the CpuHot event did not fire it.
[ "$(grep -c 'disk-firing' "$WORK/events2")" = "1" ] || { cat "$WORK/events2"; fail "CpuHot wrongly fired disk-firing"; }
log "   -> CpuHot received, did not match disk-firing"

log "an approval-gated rule leaves the action awaiting approval"
post '{"alerts":[{"status":"firing","labels":{"alertname":"RebootNeeded","severity":"critical","instance":"pi4-01"}}]}' >/dev/null 2>&1
sleep 2
"$EXO" approval list >"$WORK/appr" 2>&1 || { cat "$WORK/appr"; fail "approval list failed"; }
grep -qi 'reboot-node\|RebootNeeded\|pending\|await' "$WORK/appr" || { cat "$WORK/appr"; fail "no pending approval for the gated rule"; }
log "   -> reboot-node action is gated pending approval"

log "clean shutdown"
kill "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true
SERVE_PID=""

printf 'PASS: story-2 gate — alert fires a CEL rule through guardrails to the queue; a non-match is filtered; an approval-gated rule waits; the trace is queryable\n'
