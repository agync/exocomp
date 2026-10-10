#!/bin/sh
# Snapshot and restore the capture data dir. The snapshot is a local iteration
# cache, never a committed artifact: seeded timestamps drift, and a stale
# snapshot produces captures that look subtly wrong, which is worse than a
# failure.
#   snapshot.sh save [name]
#   snapshot.sh restore [name]
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$HERE/lib/common.sh"

name=${2:-default}
SNAPDIR="${EXOCOMP_CAPTURE_SNAPSHOTS:-$SCRATCH/snapshots}"

case "${1:-}" in
    save)
        "$DAEMON" stop >/dev/null 2>&1 || true
        mkdir -p "$SNAPDIR"
        tar -C "$SCRATCH" -czf "$SNAPDIR/$name.tgz" exocomp tokens.env
        log "saved snapshot $name → $SNAPDIR/$name.tgz"
        ;;
    restore)
        [ -f "$SNAPDIR/$name.tgz" ] || die "no snapshot named $name in $SNAPDIR"
        "$DAEMON" stop >/dev/null 2>&1 || true
        rm -rf "$SCRATCH/exocomp" "$SCRATCH/tokens.env"
        tar -C "$SCRATCH" -xzf "$SNAPDIR/$name.tgz"
        EXOCOMP_E2E_DATA="$SCRATCH" EXOCOMP_E2E_ADDR="$CAPTURE_ADDR" "$DAEMON" start --keep-data >/dev/null
        . "$SCRATCH/tokens.env"
        reload_daemon
        log "restored snapshot $name"
        ;;
    list)
        ls -1 "$SNAPDIR" 2>/dev/null || log "no snapshots yet"
        ;;
    *) echo "usage: $0 {save|restore|list} [name]" >&2; exit 2 ;;
esac
