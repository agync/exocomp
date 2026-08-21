#!/bin/sh
set -eu

VERSION=0.0.1

usage() {
    cat <<EOF
install.sh $VERSION - exocomp installer

Usage: install.sh [MODE] [OPTIONS]

Modes:
  install    Install exocomp (default)
  restore    Restore state from a backup
  rollback   Roll back to the previous release

Options:
  --help     Show this help and exit

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

for arg in "$@"; do
    case "$arg" in
        --help)
            usage
            exit 0
            ;;
        install|restore|rollback)
            mode="$arg"
            ;;
        *)
            echo "install.sh: unknown argument: $arg" >&2
            usage >&2
            exit 1
            ;;
    esac
done

tier=$(detect_tier)

if [ "$tier" = none ]; then
    echo "install.sh: neither podman nor docker found on PATH" >&2
    exit 1
fi

echo "install.sh $VERSION: tier=$tier mode=$mode"

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
