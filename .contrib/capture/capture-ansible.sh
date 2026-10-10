#!/bin/sh
# Stage B: the managed-node captures. The design renders in stage A are SQLite
# state and need no VM at all; these need a node that actually answers SSH,
# because the point of the capture is a populated inventory and a real run.
#
#   capture-ansible.sh [--keep-vm]
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$HERE/lib/common.sh"

KEEP_VM=0
while [ $# -gt 0 ]; do
    case "$1" in
        --keep-vm) KEEP_VM=1; shift ;;
        *) echo "usage: $0 [--keep-vm]" >&2; exit 2 ;;
    esac
done

[ -x "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" ] || die "run 'make build' first"
command -v limactl >/dev/null 2>&1 || die "limactl not on PATH"

trap '[ "$KEEP_VM" = 1 ] || "$HERE/lib/vms.sh" ansible down' EXIT
"$HERE/lib/vms.sh" ansible up

capture_daemon_stop
rm -rf "$SCRATCH/exocomp" "$SCRATCH/tokens.env" "$SCRATCH/daemon.pid"
capture_daemon_start
. "$HERE/lib/seed.sh"

# The node's SSH key has to be in exocomp's identity store before an inventory
# can name it, and the inventory itself is what the captures show.
cli ssh-identity add node0 --host 127.0.0.1 --port 2222 \
    --identity "$HOME/.lima/$NODE_INSTANCE/ssh.config" >/dev/null 2>&1 ||
    log "warn: ssh-identity add failed; the inventory capture will be empty"

. "$HERE/lib/verify.sh"
browser_ok || die "playwright-cli unavailable"
browser_login "$EXOCOMP_CAPTURE_BASE"
force_dark_theme

shot /sources raw-ansible-sources
log "ansible captures in $SHOTS"
