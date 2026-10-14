#!/bin/sh
# Promote raw shots into the committed renders: annotate the artboard band the
# originals carry, and write the eight committed web renders.
#
#   render-all.sh [--shots DIR] [--out DIR]
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$HERE/lib/common.sh"

SHOTS_DIR="$SHOTS"
OUT_DIR="$OUT"
while [ $# -gt 0 ]; do
    case "$1" in
        --shots) SHOTS_DIR=$2; shift 2 ;;
        --out) OUT_DIR=$2; shift 2 ;;
        *) echo "usage: $0 [--shots DIR] [--out DIR]" >&2; exit 2 ;;
    esac
done

[ -d "$SHOTS_DIR" ] || die "no shots dir at $SHOTS_DIR; run capture-renders.sh first"
mkdir -p "$OUT_DIR"

# promote <src> <dest> [id] [title] [subtitle]
# An absent subtitle leaves the band with just the id and title.
promote() {
    src=$1; dest=$2; id=${3:-}; title=${4:-}; sub=${5:-}
    [ -f "$SHOTS_DIR/$src" ] || die "missing shot $src"
    if [ -n "$id" ]; then
        annotate "$SHOTS_DIR/$src" "$id" "$title" "$sub" "$OUT_DIR/$dest"
    else
        cp "$SHOTS_DIR/$src" "$OUT_DIR/$dest"
    fi
    log "  $dest"
}

log "promoting renders into $OUT_DIR"

promote final-1a-shell-dashboard.png 1a-shell-dashboard.png \
    1a "App shell + Dashboard" \
    "grouped nav · attention-first cards · powerline speaks signal"
promote final-1b-checks.png 1b-checks.png \
    1b "Checks — card per check, target grid" \
    "reachability ≠ location · Netgroups fold into a tab · actions demoted"
promote raw-1c-sources.png 1c-sources.png \
    1c "Sources — row is the link, actions live on detail" \
    "never-synced ≠ wait state, not a clash"
promote raw-1d-source-detail.png 1d-source-detail.png \
    1d "Source detail — read first, launch is a focused drawer" \
    "native dialog · options collapsed · privilege isolated"
promote raw-1e-secrets.png 1e-secrets.png \
    1e "Secrets — metadata only, said out loud" \
    "44px rows in a padded tray · rotate is quiet, remove is guarded"
promote final-1a-shell-dashboard.png dashboard-dark.png
promote light-dashboard.png dashboard-light.png

# 1f is the one render the rig does not produce: the shared-vocabulary sheet
# shows every state chip at once, which no live surface does. It is a design
# artifact rather than a capture, so it is left as committed.

log "done"
