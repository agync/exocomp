#!/bin/sh
set -eu

VERSION=0.0.1

usage() {
    cat <<EOF
install.sh $VERSION - exocomp installer

Usage: install.sh [MODE] [OPTIONS]

Modes (positional or flag form):
  install                Install exocomp (default)
  restore | --restore    Restore state from a backup
  rollback | --rollback  Roll back to the previous release

Options:
  --from URI             With restore: target uri, or kind:uri
                         (restic_local|restic_s3|git)
  --dest DIR             With restore: data dir to rebuild (default \$EXOCOMP_DATA_DIR
                         or /var/lib/exocomp)
  --identity FILE        With restore: age identity file holding the recovery key
  --image-tag TAG        With rollback: pin this image tag
  --help                 Show this help and exit

Restore runs "exocomp restore" when an exocomp binary is available: on PATH, or
named by \$EXOCOMP_BIN. Without one it prints what it would do, because the
binary is what knows how to derive the repository password from the keyring.

Never invoked with sudo: rootless podman or docker only.
EOF
}

detect_tier() {
    if command -v podman >/dev/null 2>&1; then
        echo "podman"
    elif command -v docker >/dev/null 2>&1; then
        echo "docker"
    else
        echo "none"
    fi
}

mode=install
image_tag=""
from=""
dest=""
identity=""
data_dir=${EXOCOMP_DATA_DIR:-/var/lib/exocomp}

while [ $# -gt 0 ]; do
    case "$1" in
        --help)
            usage
            exit 0
            ;;
        install|restore|rollback)
            mode="$1"
            ;;
        --restore)
            mode=restore
            ;;
        --rollback)
            mode=rollback
            ;;
        --image-tag)
            [ $# -ge 2 ] || { echo "install.sh: --image-tag requires a value" >&2; exit 1; }
            image_tag="$2"
            shift
            ;;
        --from)
            [ $# -ge 2 ] || { echo "install.sh: --from requires a value" >&2; exit 1; }
            from="$2"
            shift
            ;;
        --dest)
            [ $# -ge 2 ] || { echo "install.sh: --dest requires a value" >&2; exit 1; }
            dest="$2"
            shift
            ;;
        --identity)
            [ $# -ge 2 ] || { echo "install.sh: --identity requires a value" >&2; exit 1; }
            identity="$2"
            shift
            ;;
        *)
            echo "install.sh: unknown argument: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
    shift
done

if [ -n "$image_tag" ] && [ "$mode" != rollback ]; then
    echo "install.sh: --image-tag is only valid with rollback" >&2
    exit 1
fi

if [ "$mode" != restore ]; then
    for opt in from dest identity; do
        eval "value=\$$opt"
        if [ -n "$value" ]; then
            echo "install.sh: --$opt is only valid with restore" >&2
            exit 1
        fi
    done
fi

[ -n "$dest" ] || dest="$data_dir"

exocomp_bin=""
if [ -n "${EXOCOMP_BIN:-}" ]; then
    exocomp_bin="$EXOCOMP_BIN"
elif command -v exocomp >/dev/null 2>&1; then
    exocomp_bin=$(command -v exocomp)
fi

tier=$(detect_tier)

if [ "$tier" = none ]; then
    echo "install.sh: neither podman nor docker found on PATH" >&2
    exit 1
fi

echo "install.sh $VERSION: tier=$tier mode=$mode${image_tag:+ image_tag=$image_tag}"

case "$mode" in
    install)
        echo "would install exocomp via Quadlet ($tier rootless), pulling packaging/Containerfile image and enabling the systemd unit"
        ;;
    restore)
        if [ -z "$exocomp_bin" ]; then
            echo "would restore $dest from ${from:-a backup target} and restart the unit"
            echo "install.sh: no exocomp binary found; set EXOCOMP_BIN or install first" >&2
            exit 0
        fi
        if [ -z "$from" ]; then
            echo "install.sh: restore requires --from <uri-or-kind:uri>" >&2
            exit 1
        fi
        set -- restore --from "$from" --dest "$dest"
        if [ -n "$identity" ]; then
            set -- "$@" --identity "$identity"
        fi
        echo "install.sh: exec $exocomp_bin $*"
        "$exocomp_bin" "$@"
        ;;
    rollback)
        echo "would roll back the running unit to the previous release image"
        ;;
esac
