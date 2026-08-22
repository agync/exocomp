#!/bin/sh
# M3c upgrade/rollback gate: self-upgrade as a step-ledger choreography.
#   1. upgrade to a GOOD staged binary swaps it in; `version` reports the new value,
#      the ledger shows all five steps succeeded.
#   2. upgrade to a DELIBERATELY BROKEN staged binary (fails version/doctor) ROLLS
#      BACK — the old binary and the pre-upgrade data snapshot are restored,
#      `version` still reports the value from step 1, and `doctor` is healthy.
#   3. `exocomp rollback` restores the prior binary on demand.
# No sudo anywhere: everything happens under a scratch data dir and a scratch bin
# dir. Run from the repo root.
set -eu

ROOT=$(git rev-parse --show-toplevel)
MODULE=exocomp.dev/exocomp
WORK=$(mktemp -d)
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export XDG_DATA_HOME="$WORK/data"
BIN="$WORK/bin"
mkdir -p "$BIN"
EXO="$BIN/exocomp"

build() { # build <out> <version>
    CGO_ENABLED=0 go build -C "$ROOT" \
        -ldflags "-X $MODULE/internal/version.Version=$2" -o "$1" ./cmd/exocomp
}

log "build the live binary (v1.0.0-gate) and init"
build "$EXO" "1.0.0-gate"
"$EXO" init >"$WORK/kit.txt"
"$EXO" version | grep -q '1.0.0-gate' || fail "live binary is not v1.0.0-gate"

# ---------------------------------------------------------------------------
log "1. upgrade to a GOOD staged binary (v2.0.0-gate)"
build "$WORK/new-good" "2.0.0-gate"
"$EXO" upgrade "$WORK/new-good" >"$WORK/up-good.out" 2>&1 \
    || { cat "$WORK/up-good.out"; fail "good upgrade returned an error"; }
cat "$WORK/up-good.out"
# every step of the ledger succeeded
for step in upgrade.snapshot upgrade.stage upgrade.verify upgrade.swap upgrade.health; do
    grep -E "$step +succeeded" "$WORK/up-good.out" >/dev/null \
        || fail "step $step did not succeed"
done
"$EXO" version | grep -q '2.0.0-gate' || fail "after a good upgrade version is not v2.0.0-gate"
log "   -> now running $("$EXO" version)"

# ---------------------------------------------------------------------------
log "2. upgrade to a DELIBERATELY BROKEN staged binary (fails doctor)"
printf '#!/bin/sh\ncase "$1" in version) echo broken ;; doctor) exit 1 ;; *) exit 0 ;; esac\n' \
    >"$WORK/new-broken"
chmod +x "$WORK/new-broken"
set +e
"$EXO" upgrade "$WORK/new-broken" >"$WORK/up-broken.out" 2>&1
BROKEN_RC=$?
set -e
cat "$WORK/up-broken.out"
[ "$BROKEN_RC" -ne 0 ] || fail "upgrade to a broken binary should have failed"
grep -q 'upgrade.verify' "$WORK/up-broken.out" || fail "the broken run did not fail at verify"
grep -q 'rolled back' "$WORK/up-broken.out" || fail "the broken run did not report a rollback"
# the rollback left the system exactly as before: still v2.0.0-gate and healthy
"$EXO" version | grep -q '2.0.0-gate' || fail "rollback did not leave v2.0.0-gate live"
"$EXO" doctor >/dev/null 2>&1 || fail "the system is not healthy after the rollback"
log "   -> version still $("$EXO" version), doctor healthy"

# ---------------------------------------------------------------------------
log "3. exocomp rollback restores the prior binary on demand"
"$EXO" rollback >"$WORK/rollback.out" 2>&1 || { cat "$WORK/rollback.out"; fail "rollback errored"; }
cat "$WORK/rollback.out"
grep -q 'restored prior binary' "$WORK/rollback.out" || fail "rollback did not restore the binary"
"$EXO" version | grep -q '1.0.0-gate' || fail "rollback did not return to v1.0.0-gate"
"$EXO" doctor >/dev/null 2>&1 || fail "the system is not healthy after rollback"
log "   -> version back to $("$EXO" version)"

log "ALL UPGRADE/ROLLBACK GATE CHECKS PASSED"
