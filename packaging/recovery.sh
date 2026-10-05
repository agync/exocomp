#!/bin/sh
# Bare-metal recovery: rebuild a data dir from a backup with no exocomp code in
# the path (AGENTS.md invariant 7 — disaster recovery never depends on the
# project's own code). Run it on a dead host, before any exocomp binary exists.
# With an exocomp binary on PATH (or $EXOCOMP_BIN) it delegates to `exocomp
# restore`; without one it runs the recovery card's chain by hand with age,
# openssl, restic, and coreutils — so a lost binary is not a lost restore.
#
# Installing the daemon itself is not this script's job: the deployment is the
# shipped image plus the Quadlet units, then `exocomp init` (docs/operations/
# bootstrap.md). Wrapping host.key into a systemd credential is documented there
# too. This script is versioned separately from exocompd on purpose: it must
# keep working when the binary does not.
set -eu

VERSION=0.0.1

usage() {
    cat <<EOF
recovery.sh $VERSION - rebuild a data dir from a backup

Usage: recovery.sh [OPTIONS]

Options:
  --from URI         Target uri, or kind:uri (restic_local|restic_s3|git)
  --dest DIR         Data dir to rebuild (default \$EXOCOMP_DATA_DIR or
                     /var/lib/exocomp)
  --identity FILE    Age identity file holding the recovery key
  --keyring FILE     keyring.age, when it cannot be read from the target root
                     (any non-local target)
  --target-name NAME The name the repository password was derived from; read
                     from RECOVERY.md when omitted
  --help             Show this help and exit

Restore runs "exocomp restore" when a binary is available: on PATH, or named by
\$EXOCOMP_BIN. Without one it runs the same chain by hand with age, openssl,
restic and coreutils - the chain printed on the recovery card.

By hand, keyring.age comes from the target root: copied from the directory for a
local target, and named with --keyring for an s3 or git target, which you fetch
yourself with whatever reaches that store.

Never invoked with sudo: rootless podman or docker only.
EOF
}

fail() {
    echo "recovery.sh: $*" >&2
    exit 1
}

# ask prints a prompt and reads one line, but only when a human is there to
# answer. A DR script that blocks forever on a closed stdin is a DR script that
# fails at 3am.
ask() {
    [ -t 0 ] || return 1
    printf '%s' "$1" >&2
    read -r reply || return 1
    [ -n "$reply" ] || return 1
    echo "$reply"
}

from=""
dest=""
identity=""
keyring=""
target_name=""
data_dir=${EXOCOMP_DATA_DIR:-/var/lib/exocomp}

while [ $# -gt 0 ]; do
    case "$1" in
        --help)
            usage
            exit 0
            ;;
        --from)
            [ $# -ge 2 ] || fail "--from requires a value"
            from="$2"
            shift
            ;;
        --dest)
            [ $# -ge 2 ] || fail "--dest requires a value"
            dest="$2"
            shift
            ;;
        --identity)
            [ $# -ge 2 ] || fail "--identity requires a value"
            identity="$2"
            shift
            ;;
        --keyring)
            [ $# -ge 2 ] || fail "--keyring requires a value"
            keyring="$2"
            shift
            ;;
        --target-name)
            [ $# -ge 2 ] || fail "--target-name requires a value"
            target_name="$2"
            shift
            ;;
        *)
            echo "recovery.sh: unknown argument: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
    shift
done

[ -n "$dest" ] || dest="$data_dir"

exocomp_bin=""
if [ -n "${EXOCOMP_BIN:-}" ]; then
    exocomp_bin="$EXOCOMP_BIN"
elif command -v exocomp >/dev/null 2>&1; then
    exocomp_bin=$(command -v exocomp)
fi

# repo_path strips the kind prefix a target uri may carry, so "restic_local:/srv/x"
# and "/srv/x" name the same repository.
repo_path() {
    case "$1" in
        restic_local:*) echo "${1#restic_local:}" ;;
        local:*)        echo "${1#local:}" ;;
        restic_s3:*)    echo "${1#restic_s3:}" ;;
        *)              echo "$1" ;;
    esac
}

# manifest_sha reads one file's recorded digest out of MANIFEST.json. The file
# is pretty-printed by the writer, one field per line, so awk is enough and a
# restore does not need a JSON parser on the recovery host.
manifest_sha() {
    awk -v want="$1" '
        $1 == "\"name\":" {
            name = $2
            gsub(/^"|",?$/, "", name)
        }
        $1 == "\"sha256\":" && name == want {
            sha = $2
            gsub(/^"|",?$/, "", sha)
            print sha
            exit
        }
    ' "$2"
}

verify_file() {
    want=$(manifest_sha "$1" "$3/MANIFEST.json")
    [ -n "$want" ] || fail "MANIFEST.json records no sha256 for $1"
    got=$(sha256sum "$2" | cut -d' ' -f1)
    [ "$got" = "$want" ] || fail "$1: sha256 $got, manifest says $want"
}

# manual_restore walks the recovery card's chain with no exocomp code in it:
# unwrap the DEK with age, derive the repository password with openssl, restore
# the newest snapshot with restic. Every step's failure is fatal.
manual_restore() {
    require_tools age openssl restic od tr base64 sha256sum awk

    [ -n "$from" ] || from=$(ask "target uri (or kind:uri): ") \
        || fail "restore requires --from <uri-or-kind:uri>"
    [ -n "$identity" ] || identity=$(ask "age identity file holding the recovery key: ") \
        || fail "restore without an exocomp binary requires --identity <recovery-key-file>"
    [ -r "$identity" ] || fail "identity file $identity is not readable"

    repo=$(repo_path "$from")

    case "$from" in
        s3:*|restic_s3:*|git:*|https://*|ssh://*|git@*)
            [ -n "$keyring" ] || fail "a $from target cannot be read as a directory: fetch keyring.age from the target root yourself and pass --keyring FILE"
            ;;
        *)
            if [ -z "$keyring" ]; then
                [ -d "$repo" ] || fail "target root $repo is not a directory"
                keyring="$repo/keyring.age"
            fi
            if [ -z "$target_name" ] && [ -r "$repo/RECOVERY.md" ]; then
                target_name=$(sed -n 's/^target-name: *//p' "$repo/RECOVERY.md" | head -n 1)
            fi
            ;;
    esac
    [ -r "$keyring" ] || fail "keyring.age not readable at $keyring"

    [ -n "$target_name" ] || target_name=$(ask "target name the repository password was derived from: ") \
        || fail "target name unknown: pass --target-name, or restore from a target root holding RECOVERY.md"

    # 0077 so nothing below is readable by anyone else, DEK included.
    umask 0077
    work=$(mktemp -d "${TMPDIR:-/tmp}/exocomp-restore.XXXXXX")
    trap 'rm -rf "$work"' EXIT INT TERM

    echo "recovery.sh: unwrapping keyring.age with $identity"
    age -d -i "$identity" "$keyring" > "$work/dek.bin" \
        || fail "age could not unwrap $keyring with $identity"

    echo "recovery.sh: deriving the repository password for target $target_name"
    hex() { od -An -tx1 -v | tr -d ' \n'; }
    openssl kdf -keylen 32 -kdfopt digest:SHA256 \
        -kdfopt "hexkey:$(hex < "$work/dek.bin")" \
        -kdfopt "hexsalt:$(printf 'exocomp/v1' | hex)" \
        -kdfopt "hexinfo:$(printf 'restic-repo:%s' "$target_name" | hex)" \
        -binary HKDF | base64 > "$work/restic.pass" \
        || fail "openssl could not derive the repository password"

    echo "recovery.sh: restoring the newest snapshot from $repo"
    RESTIC_PASSWORD_FILE="$work/restic.pass" restic -r "$repo" \
        restore latest --target "$work/tree" \
        || fail "restic could not restore the newest snapshot from $repo"

    # restic recreates absolute paths under --target, so the staging tree lands
    # somewhere below it. MANIFEST.json marks the spot.
    staged=$(find "$work/tree" -name MANIFEST.json -print | head -n 1)
    [ -n "$staged" ] || fail "the restored snapshot holds no MANIFEST.json"
    staged=$(dirname "$staged")

    for name in snapshot.db keyring.age; do
        [ -f "$staged/$name" ] || fail "the restored snapshot holds no $name"
        verify_file "$name" "$staged/$name" "$staged"
    done

    for name in exocomp.db keyring.age host.key; do
        if [ -e "$dest/$name" ]; then
            fail "$dest/$name exists: refusing to restore over live state"
        fi
    done
    mkdir -p "$dest"
    chmod 0700 "$dest"

    cp "$staged/snapshot.db" "$dest/exocomp.db"
    cp "$staged/keyring.age" "$dest/keyring.age"
    cp "$staged/MANIFEST.json" "$dest/MANIFEST.json"
    chmod 0600 "$dest/exocomp.db" "$dest/keyring.age" "$dest/MANIFEST.json"

    echo "recovery.sh: restored $dest from $repo (target $target_name), sha256 verified"
    echo "recovery.sh: host.key is NOT restored; this host reads secrets with the recovery identity until it is re-keyed"
}

require_tools() {
    for tool in "$@"; do
        command -v "$tool" >/dev/null 2>&1 || fail "$tool is required for a manual restore and is not on PATH"
    done
}

# No container runtime is required to restore: recovery must not depend on
# anything but the files and off-the-shelf tools (AGENTS.md invariant 7).
echo "recovery.sh $VERSION: dest=$dest"
if [ -z "$exocomp_bin" ]; then
    echo "recovery.sh: no exocomp binary; running the recovery card's chain by hand" >&2
    manual_restore
else
    [ -n "$from" ] || fail "restore requires --from <uri-or-kind:uri>"
    set -- restore --from "$from" --dest "$dest"
    if [ -n "$identity" ]; then
        set -- "$@" --identity "$identity"
    fi
    if [ -n "$target_name" ]; then
        set -- "$@" --target-name "$target_name"
    fi
    echo "recovery.sh: exec $exocomp_bin $*"
    "$exocomp_bin" "$@"
fi
