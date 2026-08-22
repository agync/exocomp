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
  harden-host-key        Wrap host.key into a systemd credential (TPM-sealed
                         when a TPM is present), off the data volume (finding E1)

Options:
  --from URI             With restore: target uri, or kind:uri
                         (restic_local|restic_s3|git)
  --dest DIR             With restore: data dir to rebuild (default \$EXOCOMP_DATA_DIR
                         or /var/lib/exocomp)
  --identity FILE        With restore: age identity file holding the recovery key
  --keyring FILE         With restore: keyring.age, when it cannot be read from
                         the target root (any non-local target)
  --target-name NAME     With restore: the name the repository password was
                         derived from; read from RECOVERY.md when omitted
  --image-tag TAG        With rollback: pin this image tag
  --help                 Show this help and exit

Restore runs "exocomp restore" when an exocomp binary is available: on PATH, or
named by \$EXOCOMP_BIN. Without one it runs the same chain by hand with age,
openssl, restic and coreutils - the chain printed on the recovery card - so a
lost binary is not a lost restore.

By hand, keyring.age comes from the target root: copied from the directory for
a local target, and named with --keyring for an s3 or git target, which you
fetch yourself with whatever reaches that store.

Never invoked with sudo: rootless podman or docker only.
EOF
}

fail() {
    echo "install.sh: $*" >&2
    exit 1
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

require_tools() {
    for tool in "$@"; do
        command -v "$tool" >/dev/null 2>&1 || fail "$tool is required for a manual restore and is not on PATH"
    done
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

mode=install
image_tag=""
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
        install|restore|rollback)
            mode="$1"
            ;;
        harden-host-key|--harden-host-key)
            mode=harden-host-key
            ;;
        --restore)
            mode=restore
            ;;
        --rollback)
            mode=rollback
            ;;
        --image-tag)
            [ $# -ge 2 ] || fail "--image-tag requires a value"
            image_tag="$2"
            shift
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
            echo "install.sh: unknown argument: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
    shift
done

if [ -n "$image_tag" ] && [ "$mode" != rollback ]; then
    fail "--image-tag is only valid with rollback"
fi

if [ "$mode" != restore ]; then
    for opt in from dest identity keyring target_name; do
        eval "value=\$$opt"
        if [ -n "$value" ]; then
            fail "--$(echo "$opt" | tr _ -) is only valid with restore"
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

    echo "install.sh: unwrapping keyring.age with $identity"
    age -d -i "$identity" "$keyring" > "$work/dek.bin" \
        || fail "age could not unwrap $keyring with $identity"

    echo "install.sh: deriving the repository password for target $target_name"
    hex() { od -An -tx1 -v | tr -d ' \n'; }
    openssl kdf -keylen 32 -kdfopt digest:SHA256 \
        -kdfopt "hexkey:$(hex < "$work/dek.bin")" \
        -kdfopt "hexsalt:$(printf 'exocomp/v1' | hex)" \
        -kdfopt "hexinfo:$(printf 'restic-repo:%s' "$target_name" | hex)" \
        -binary HKDF | base64 > "$work/restic.pass" \
        || fail "openssl could not derive the repository password"

    echo "install.sh: restoring the newest snapshot from $repo"
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

    echo "install.sh: restored $dest from $repo (target $target_name), sha256 verified"
    echo "install.sh: host.key is NOT restored; this host reads secrets with the recovery identity until it is re-keyed"
}

# wrap_host_key moves the host key off the data volume into a systemd credential
# (finding E1). systemd-creds encrypts it with a host secret
# (/var/lib/systemd/credential.secret) or, when a TPM 2.0 is present, seals it to
# the TPM (--with-key=host+tpm2) — neither of which lives in the data volume, so
# a stolen volume holds only the opaque host.key.cred. The quadlet's
# LoadCredentialEncrypted= decrypts it back to $CREDENTIALS_DIRECTORY/host.key at
# start. The plaintext key is deleted once the credential is written.
wrap_host_key() {
    plaintext="$data_dir/host.key"
    cred="$data_dir/host.key.cred"

    command -v systemd-creds >/dev/null 2>&1 \
        || fail "systemd-creds is required to wrap the host key and is not on PATH"
    [ -f "$plaintext" ] || fail "$plaintext does not exist; nothing to wrap (already wrapped?)"

    if [ -e /dev/tpmrm0 ]; then
        with_key="host+tpm2"
        echo "install.sh: TPM 2.0 present — sealing the host key to this box (--with-key=$with_key)"
    else
        with_key="host"
        echo "install.sh: no TPM — wrapping the host key with the systemd host secret (--with-key=$with_key)"
    fi

    umask 077
    systemd-creds encrypt --name=host.key --with-key="$with_key" "$plaintext" "$cred" \
        || fail "systemd-creds could not encrypt the host key (try --with-key=host if TPM sealing failed)"
    rm -f "$plaintext"
    echo "install.sh: host key wrapped to $cred; plaintext removed. The daemon reads it via the quadlet credential."
}

case "$mode" in
    install)
        tier=$(detect_tier)
        if [ "$tier" = none ]; then
            fail "neither podman nor docker found on PATH"
        fi
        echo "install.sh $VERSION: tier=$tier mode=$mode"
        echo "would install exocomp via Quadlet ($tier rootless), pulling packaging/Containerfile image and enabling the systemd unit"
        echo "install.sh: after init, run \`install.sh harden-host-key\` to move host.key off the volume into a systemd credential"
        ;;
    harden-host-key)
        echo "install.sh $VERSION: mode=harden-host-key data_dir=$data_dir"
        wrap_host_key
        ;;
    restore)
        # No container runtime is required to restore: recovery must not depend
        # on anything but the files and off-the-shelf tools (AGENTS.md 7).
        echo "install.sh $VERSION: mode=restore dest=$dest"
        if [ -z "$exocomp_bin" ]; then
            echo "install.sh: no exocomp binary; running the recovery card's chain by hand" >&2
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
            echo "install.sh: exec $exocomp_bin $*"
            "$exocomp_bin" "$@"
        fi
        ;;
    rollback)
        tier=$(detect_tier)
        if [ "$tier" = none ]; then
            fail "neither podman nor docker found on PATH"
        fi
        echo "install.sh $VERSION: tier=$tier mode=$mode${image_tag:+ image_tag=$image_tag}"
        echo "would roll back the running unit to the previous release image"
        ;;
esac
