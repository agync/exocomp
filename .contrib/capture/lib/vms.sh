#!/bin/sh
# Lima lifecycle for the capture rig. Both VMs the captures need already exist
# on this host; the rig only starts and stops them, never creates or deletes.
#
#   vms.sh ansible up     start the dummy managed node, wait for SSH
#   vms.sh ansible down
#   vms.sh k3s up         start the k3s cluster, wait for the API
#   vms.sh k3s down
set -eu

NODE_INSTANCE="${EXOCOMP_CAPTURE_NODE:-exocomp-node0}"
K3S_INSTANCE="${EXOCOMP_CAPTURE_K3S:-exocomp-k3s}"
NODE_SSH_PORT=2222

log() { printf 'vms: %s\n' "$*" >&2; }

node_up() {
    limactl list --format '{{.Name}} {{.Status}}' 2>/dev/null |
        grep -q "^$NODE_INSTANCE Running" || limactl start "$NODE_INSTANCE" >/dev/null 2>&1
    i=0
    until timeout 5 ssh -p "$NODE_SSH_PORT" -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null -o BatchMode=yes \
        "${EXOCOMP_CAPTURE_NODE_USER:-$(id -un)}@127.0.0.1" true >/dev/null 2>&1; do
        i=$((i + 1)); [ "$i" -lt 30 ] || die "node $NODE_INSTANCE never became reachable on $NODE_SSH_PORT"
        sleep 2
    done
    log "managed node up on 127.0.0.1:$NODE_SSH_PORT"
}

k3s_up() {
    limactl list --format '{{.Name}} {{.Status}}' 2>/dev/null |
        grep -q "^$K3S_INSTANCE Running" || limactl start "$K3S_INSTANCE" >/dev/null 2>&1
    cfg="$HOME/.lima/$K3S_INSTANCE/copied-from-guest/kubeconfig.yaml"
    [ -f "$cfg" ] || die "kubeconfig not at $cfg; is k3s installed in the VM?"
    log "k3s up; kubeconfig at $cfg"
}

case "${1:-}" in
    ansible) [ "${2:-}" = up ] && node_up || limactl stop "$NODE_INSTANCE" >/dev/null 2>&1 ;;
    k3s) [ "${2:-}" = up ] && k3s_up || limactl stop "$K3S_INSTANCE" >/dev/null 2>&1 ;;
    *) echo "usage: $0 {ansible|k3s} {up|down}" >&2; exit 2 ;;
esac
