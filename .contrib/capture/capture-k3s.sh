#!/bin/sh
# Stage C: the live k3s apply capture. This is the one capture that genuinely
# needs a lima VM, because the point is a real plan and apply against a real
# cluster, not a row in SQLite.
#
#   capture-k3s.sh [--keep-vm]
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

trap '[ "$KEEP_VM" = 1 ] || "$HERE/lib/vms.sh" k3s down' EXIT
"$HERE/lib/vms.sh" k3s up

capture_daemon_stop
rm -rf "$SCRATCH/exocomp" "$SCRATCH/tokens.env" "$SCRATCH/daemon.pid"
capture_daemon_start

# S11 in the tofu rig is the whole scenario: register the project, declare
# egress, plan, apply, confirm. Reuse it rather than restate it.
RIG="$ROOT/.contrib/tofu-e2e"
export EXOCOMP_E2E_DATA="$SCRATCH"
export EXOCOMP_E2E_ADDR="$CAPTURE_ADDR"
"$RIG/run-e2e.sh" --scenario S11 --project b-k8s >/dev/null 2>&1 ||
    die "S11 failed; see .agents/brain/tofu-e2e/results"

. "$HERE/lib/verify.sh"
browser_ok || die "playwright-cli unavailable"
browser_login "$EXOCOMP_CAPTURE_BASE"
force_dark_theme

shot /workspaces raw-k3s-workspaces
log "k3s captures in $SHOTS"
