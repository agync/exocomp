#!/bin/sh
# Prepare the ansible-e2e source checkout for THIS run: re-derive the lima SSH
# endpoint (the forwarded port changes across VM restarts — never cache it),
# write the per-run ssh config + inventory into the checkout, and start the
# node if it is stopped. The inventory host name matches the ssh config stanza
# (lima-exocomp-node0) so the config's host keys/port apply.
#
# Usage: lib/prepare.sh [--ensure-node]
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
RIG="$ROOT/.contrib/ansible-e2e"
INSTANCE="${EXOCOMP_E2E_NODE:-exocomp-node0}"
INFRA="$RIG/projects/infra"

log() { printf 'prepare: %s\n' "$*" >&2; }

if [ "${1:-}" = "--ensure-node" ]; then
    state=$(limactl list --format '{{.Status}}' "$INSTANCE" 2>/dev/null || true)
    if [ "$state" != "Running" ]; then
        log "starting $INSTANCE (first boot downloads a Fedora image, allow up to 15 minutes)"
        limactl start --name "$INSTANCE" -y "$ROOT/test/lima/node-fedora.yaml"
    fi
fi

tries=0
until limactl shell "$INSTANCE" -- true >/dev/null 2>&1; do
    tries=$((tries + 1))
    [ "$tries" -lt 90 ] || { log "ssh not ready after $tries attempts"; exit 1; }
    sleep 5
done
log "$INSTANCE ssh ready"

limactl show-ssh --format=config "$INSTANCE" >"$INFRA/ssh_config" 2>/dev/null
host=$(limactl show-ssh --format=config "$INSTANCE" 2>/dev/null | sed -n 's/^  Hostname *//p')
port=$(limactl show-ssh --format=config "$INSTANCE" 2>/dev/null | sed -n 's/^  Port *//p')
[ -n "$host" ] && [ -n "$port" ] || { log "could not derive host/port from show-ssh"; exit 1; }

# The sandboxed tier grants the source checkout read-only, but the lima
# identity key lives at ~/.lima/_config/user — outside that grant — so a
# SANDBOXED run cannot load it. Copy the key into the checkout per-run (a
# gitignored rig artifact, like the ssh_config) and point the config at it.
KEY_SRC=$(limactl show-ssh --format=config "$INSTANCE" 2>/dev/null |
    sed -n 's/^  IdentityFile *"\(.*\)"/\1/p' | head -1)
: "${KEY_SRC:?could not derive the lima identity key path}"
cp "$KEY_SRC" "$INFRA/lima-identity"
chmod 600 "$INFRA/lima-identity"
sed -i "s|IdentityFile \".*\"|IdentityFile \"$INFRA/lima-identity\"|" "$INFRA/ssh_config"

cat >"$INFRA/inventory.ini" <<EOF
[node0]
lima-exocomp-node0

[node0:vars]
ansible_ssh_common_args=-F $INFRA/ssh_config
EOF

log "node0 -> $host:$port (config + identity key written to $INFRA)"
