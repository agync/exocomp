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
  --image-tag TAG        With rollback: pin this image tag
  --help                 Show this help and exit

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
        echo "would restore /var/lib/exocomp state from the given backup and restart the unit"
        ;;
    rollback)
        echo "would roll back the running unit to the previous release image"
        ;;
esac
