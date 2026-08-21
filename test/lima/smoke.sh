#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
TEMPLATE="$SCRIPT_DIR/node-fedora.yaml"
INSTANCE=exocomp-node0

log() { printf '%s\n' "$*" >&2; }
fail() { log "FAIL: $*"; exit 1; }

if [ "${1:-}" = "stop" ]; then
    limactl stop "$INSTANCE"
    exit $?
fi

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

state=$(limactl list --format '{{.Status}}' "$INSTANCE" 2>/dev/null || true)
if [ "$state" != "Running" ]; then
    log "starting $INSTANCE (first boot downloads a Fedora image, allow up to 15 minutes)"
    limactl start --name "$INSTANCE" -y "$TEMPLATE"
fi

log "waiting for ssh readiness"
tries=0
until limactl shell "$INSTANCE" -- true >/dev/null 2>&1; do
    tries=$((tries + 1))
    [ "$tries" -lt 90 ] || fail "ssh not ready after $tries attempts"
    sleep 10
done

log "== uname =="
limactl shell "$INSTANCE" -- uname -a || fail "uname failed"

if ! command -v ansible-playbook >/dev/null 2>&1; then
    log "ansible-core not found, installing user-scope via uv"
    uv tool install ansible-core
fi
export PATH="$HOME/.local/bin:$PATH"
command -v ansible >/dev/null 2>&1 || fail "ansible not on PATH after install"

ssh_config="$WORKDIR/ssh_config"
limactl show-ssh --format=config "$INSTANCE" >"$ssh_config" 2>/dev/null

host_alias="lima-$INSTANCE"
inventory="$WORKDIR/inventory.ini"
cat >"$inventory" <<EOF
[nodes]
$host_alias

[nodes:vars]
ansible_ssh_common_args=-F $ssh_config
EOF

log "== ansible ping =="
if ping_out=$(ansible -i "$inventory" nodes -m ping 2>&1) && printf '%s' "$ping_out" | grep -q '"ping": "pong"'; then
    printf '%s\n' "$ping_out"
    log "PASS: $INSTANCE reachable via ansible"
    exit 0
fi
printf '%s\n' "$ping_out"
fail "ansible ping did not report pong"
