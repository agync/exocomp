#!/bin/sh
# Stage A: the eight design renders. No lima VM of any kind, because every one
# of them is SQLite state rather than a live host.
#
#   capture-renders.sh [--out DIR] [--skip-seed] [--keep-daemon]
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$HERE/lib/common.sh"

OUT_DIR="$OUT"
SKIP_SEED=0
KEEP=0
while [ $# -gt 0 ]; do
    case "$1" in
        --out) OUT_DIR=$2; shift 2 ;;
        --skip-seed) SKIP_SEED=1; shift ;;
        --keep-daemon) KEEP=1; shift ;;
        *) echo "usage: $0 [--out DIR] [--skip-seed] [--keep-daemon]" >&2; exit 2 ;;
    esac
done

[ -x "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" ] || die "run 'make build' first"



if [ "$SKIP_SEED" = 0 ]; then
    capture_daemon_stop
    rm -rf "$SCRATCH/exocomp" "$SCRATCH/tokens.env" "$SCRATCH/daemon.pid"
    capture_daemon_start
    . "$HERE/lib/seed.sh"
    . "$HERE/lib/verify.sh"
    seed_renders
    reload_daemon
    wait_until_verified "${EXOCOMP_CAPTURE_SETTLE:-90}"
else
    capture_daemon_start --keep-data
    log "seed skipped; using the snapshot in $SCRATCH"
fi

browser_ok || die "playwright-cli unavailable; the shoot step needs it"
browser_login "$EXOCOMP_CAPTURE_BASE"
force_dark_theme

mkdir -p "$OUT_DIR"
log "capturing renders into $OUT_DIR"

shot /checks       raw-1b-checks
shot /secrets      raw-1e-secrets
shot /sources      raw-1c-sources
shot /sources/site raw-1d-source-detail
shot /             raw-1a-shell-dashboard

# The up/down split is produced by real probes, so let the sweep settle before
# the checks page and the dashboard are captured a second time and kept.
wait_until_verified "${EXOCOMP_CAPTURE_SETTLE:-90}"
shot /checks       final-1b-checks
shot /             final-1a-shell-dashboard

# dashboard-light.png is the same screen with the system theme resolving to
# light, so the dark emulation is dropped rather than inverted: the page's own
# toggle is what the light render demonstrates.
if [ "${EXOCOMP_CAPTURE_LIGHT:-1}" = "1" ]; then
    PW run-code "async page => { await page.emulateMedia({ colorScheme: 'light' }); }" >/dev/null
    shot /           light-dashboard
    force_dark_theme
fi

PW close >/dev/null 2>&1 || true

log "raw shots in $SHOTS; annotate and promote with render-all.sh"
