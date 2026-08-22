#!/bin/sh
# v1.0 acceptance gate: the five stories that gate v1.0 (docs/design/v1-acceptance.md),
# each proven by its own end-to-end gate. This runner sequences them and reports a
# single verdict. Any gate that SKIPs (a tool absent) is reported, not failed.
#
#   Story 1  "here is my git repo, go run my playbook regularly"   -> m3d-story1-gate.sh
#   Story 2  "on event/alert, run X"                               -> m3d-story2-gate.sh
#   Story 3  "run my terraform regularly and notify me of drift"   -> m3d-story3-gate.sh
#   Story 4  "load my working directory and run ansible/terraform" -> m1-gate.sh (lima) + m2-gate.sh
#   Story 5  "run terraform, then trigger dynamic inventory FROM it"-> m3c-story5-gate.sh
set -u

ROOT=$(git rev-parse --show-toplevel)
DIR="$ROOT/test/e2e"
pass=0; skip=0; fail=0
results=""

run_story() {
    label=$1; script=$2
    printf '\n########## %s (%s) ##########\n' "$label" "$script"
    out=$(sh "$DIR/$script" 2>&1); rc=$?
    printf '%s\n' "$out" | tail -3
    if [ "$rc" -ne 0 ]; then
        fail=$((fail+1)); results="$results\nFAIL  $label ($script)"
    elif printf '%s' "$out" | grep -q '^SKIP:'; then
        skip=$((skip+1)); results="$results\nSKIP  $label ($script)"
    else
        pass=$((pass+1)); results="$results\nPASS  $label ($script)"
    fi
}

run_story "Story 1 · git source + signed ref"        m3d-story1-gate.sh
run_story "Story 2 · event/alert -> run"             m3d-story2-gate.sh
run_story "Story 3 · scheduled drift -> notify"      m3d-story3-gate.sh
run_story "Story 4a · vaulted play on a lima node"   m1-gate.sh
run_story "Story 4b · daemon tofu + ansible (local)" m2-gate.sh
run_story "Story 5 · terraform -> ansible inventory" m3c-story5-gate.sh

printf '\n================ v1.0 ACCEPTANCE ================\n'
printf '%b\n' "$results"
printf '\n%d passed, %d skipped, %d failed\n' "$pass" "$skip" "$fail"
[ "$fail" -eq 0 ] || { printf 'v1.0 ACCEPTANCE: FAILED\n'; exit 1; }
printf 'v1.0 ACCEPTANCE: ALL FIVE STORIES GREEN\n'
