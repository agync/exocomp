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
                     (any non-local restic target). For a git target, a copy
                     you trust that the backup branch's keyring.age must equal
  --target-name NAME The name the repository password was derived from, and the
                     target the git tags are keyed to. Read from RECOVERY.md when
                     omitted; for a git target that name comes from the remote,
                     so pass it
  --allow-unauthenticated
                     Accept a git snapshot whose manifest tags are missing or
                     cannot be checked (a keyring that will not unwrap). Only
                     for a snapshot you are certain is yours. A tag that is
                     present and wrong is always refused.
  --image-load FILE  Load an OCI archive into the container runtime
                     (podman load -i, or docker load -i) before restore
  --image-ref REF    The container image this deployment runs (recorded for
                     the recovery chain); prints the unit's Environment= line
  --help             Show this help and exit

Restore runs "exocomp restore" when a binary is available: on PATH, or named by
\$EXOCOMP_BIN. Without one it runs the same chain by hand with age, openssl,
restic and coreutils - the chain printed on the recovery card.

By hand, keyring.age comes from the target root: copied from the directory for a
local target, and named with --keyring for an s3 target, which you fetch yourself
with whatever reaches that store.

A git target runs a longer chain: the script clones the backup branch,
authenticates the parts manifest and MANIFEST.json under the target's manifest
subkey, checks every part against the parts manifest, and decrypts them with the
identity (git, age, openssl, tar and zstd or gzip). It refuses a snapshot that
carries no tag unless --allow-unauthenticated is given. Two inputs come from the
remote unless you give them. The keyring that derives the tags' key is the
branch's own, so the tags show that the snapshot matches that keyring, not that
the keyring is yours: anyone who can push to the remote and knows a recipient's
public key can write a matching pair. Pass --keyring with a copy of keyring.age
you trust and the script refuses a branch whose keyring differs. The target name
keys the tags, so pass --target-name and the branch cannot choose which target's
tags it passes for. exocomp restore cannot pin a keyring, so --keyring makes the
script run this chain even when an exocomp binary is present.

Reach a private git remote with a credential helper, an SSH key or GIT_ASKPASS
rather than a token written into the URL. Any URL userinfo this script prints is
masked, but a token in the URL still lands in shell history and process lists.

Never invoked with sudo: rootless podman or docker only.
EOF
}

# redact hides the userinfo of every URL it reads, so a token written into a git
# remote never reaches the terminal or a log.
redact() {
    sed -E 's#([A-Za-z][A-Za-z0-9+.-]*://)[^/[:space:]]*@#\1***@#g'
}

# clean_line prints its arguments with the URL userinfo masked and every byte
# outside printable ASCII replaced by ?. Names and values that came from a
# remote can carry escape sequences or newlines, which would redraw the terminal
# or forge a line of this script's own output. say and fail print through printf
# %s, because echo reads backslash sequences in some shells.
clean_line() {
    printf '%s' "$*" | redact | LC_ALL=C tr -c ' -~' '?'
}

say() {
    printf 'recovery.sh: %s\n' "$(clean_line "$*")"
}

fail() {
    printf 'recovery.sh: %s\n' "$(clean_line "$*")" >&2
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
image_load=""
image_ref=""
allow_unauth=""
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
        --allow-unauthenticated)
            allow_unauth=1
            ;;
        --image-load)
            [ $# -ge 2 ] || fail "--image-load requires a value"
            image_load="$2"
            shift
            ;;
        --image-ref)
            [ $# -ge 2 ] || fail "--image-ref requires a value"
            image_ref="$2"
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

# Load a captured image archive into the local container runtime before any
# restore runs, so the unit has something to start from on the new host.
if [ -n "$image_load" ]; then
    [ -f "$image_load" ] || fail "image archive $image_load is not a file"
    if command -v podman >/dev/null 2>&1; then
        runtime=podman
    elif command -v docker >/dev/null 2>&1; then
        runtime=docker
    else
        fail "no container runtime found to load $image_load (need podman or docker)"
    fi
    echo "recovery.sh: loading image from $image_load with $runtime"
    "$runtime" load -i "$image_load" || fail "$runtime could not load $image_load"
fi
if [ -n "$image_ref" ]; then
    echo "recovery.sh: this deployment runs image $image_ref"
    echo "recovery.sh: set it on the unit with Environment=EXOCOMP_IMAGE=$image_ref"
fi

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

# trim strips surrounding whitespace, as "exocomp restore" does to --from.
trim() {
    printf '%s' "$1" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

# is_git_target reports whether a --from value names a git target, by the same
# rules as ParseSource and inferKind in internal/backup/restore.go: an explicit
# restic_local: or restic_s3: prefix is restic, a git: prefix is git, otherwise
# s3: is restic, a .git suffix or a git@, git://, ssh:// or https:// form is git,
# and anything else is git when it holds an objects entry once local: is
# dropped. packaging/restore_git_test.go compares the two.
is_git_target() {
    spec=$(trim "$1")
    case "$spec" in
        restic_local:?*|restic_s3:?*) return 1 ;;
        git:?*) return 0 ;;
        s3:*) return 1 ;;
        *.git|git@*|git://*|ssh://*|https://*) return 0 ;;
    esac
    [ -e "${spec#local:}/objects" ]
}

# git_url is the remote a git target names, without the git: kind prefix. A
# git:// scheme keeps its prefix, which is part of the url.
git_url() {
    spec=$(trim "$1")
    case "$spec" in
        git://*) echo "$spec" ;;
        git:?*)  echo "${spec#git:}" ;;
        *)       echo "$spec" ;;
    esac
}

# hex prints stdin as one line of lowercase hex.
hex() { od -An -tx1 -v | tr -d ' \n'; }

# unhex writes the bytes a lowercase hex string names. printf is a builtin, so
# the bytes never appear in a process argument list.
unhex() {
    rest=$1
    while [ -n "$rest" ]; do
        printf "\\$(printf '%03o' "$((0x${rest%"${rest#??}"}))")"
        rest=${rest#??}
    done
}

# hmac_pad writes the 64-byte HMAC block of a hex key xor $2 (0x36 inner, 0x5c
# outer), the key zero-padded.
hmac_pad() {
    rest=$1
    i=0
    while [ "$i" -lt 64 ]; do
        b=00
        if [ -n "$rest" ]; then
            b=${rest%"${rest#??}"}
            rest=${rest#??}
        fi
        printf "\\$(printf '%03o' "$(( 0x$b ^ $2 ))")"
        i=$((i + 1))
    done
}

# hmac_hex prints HMAC-SHA256 of file $2 under the hex key $1. openssl takes
# a key only as a command-line argument, which any local user can read while it
# runs, so the construction is done here from openssl's plain digest: the key
# reaches openssl only through its standard input, as part of the data.
hmac_hex() {
    { hmac_pad "$1" 54; cat "$2"; } | openssl dgst -sha256 -binary > "$work/hmac.inner"
    { hmac_pad "$1" 92; cat "$work/hmac.inner"; } | openssl dgst -sha256 -binary | hex
}

# hkdf_key derives 32 raw bytes from a DEK file, as the card does: HKDF-SHA256
# with the versioned salt and the given info string. It is RFC 5869 over
# hmac_hex, for one output block.
hkdf_key() {
    printf '%s\001' "$2" > "$work/hkdf.info"
    prk=$(hmac_hex "$(printf 'exocomp/v1' | hex)" "$1")
    unhex "$(hmac_hex "$prk" "$work/hkdf.info")"
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
    [ -n "$from" ] || from=$(ask "target uri (or kind:uri): ") \
        || fail "restore requires --from <uri-or-kind:uri>"
    [ -n "$identity" ] || identity=$(ask "age identity file holding the recovery key: ") \
        || fail "restore without an exocomp binary requires --identity <recovery-key-file>"
    [ -r "$identity" ] || fail "identity file $identity is not readable"

    if is_git_target "$from"; then
        manual_restore_git
        return
    fi
    require_tools age openssl restic od tr base64 sha256sum awk

    repo=$(repo_path "$from")

    case "$from" in
        s3:*|restic_s3:*)
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

    say "deriving the repository password for target $target_name"
    hkdf_key "$work/dek.bin" "restic-repo:$target_name" | base64 > "$work/restic.pass" \
        || fail "openssl could not derive the repository password"

    say "restoring the newest snapshot from $repo"
    RESTIC_PASSWORD_FILE="$work/restic.pass" restic -r "$repo" \
        restore latest --target "$work/tree" \
        || fail "restic could not restore the newest snapshot from $repo"

    # restic recreates absolute paths under --target, so the staging tree lands
    # somewhere below it. MANIFEST.json marks the spot.
    staged=$(find "$work/tree" -name MANIFEST.json -print | head -n 1)
    [ -n "$staged" ] || fail "the restored snapshot holds no MANIFEST.json"
    install_staged "$(dirname "$staged")" "$repo (target $target_name)"
}

# install_staged verifies a restored staging tree against its MANIFEST.json and
# moves the data dir into place, refusing to write over live state. $1 is the
# tree, $2 what it came from, for the closing message.
install_staged() {
    staged="$1"

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

    say "restored $dest from $2, sha256 verified"
    echo "recovery.sh: host.key is NOT restored; this host reads secrets with the recovery identity until it is re-keyed"
}

# json_str prints the first string value of a key in a pretty-printed JSON
# file. The writers emit one field per line, so sed is enough.
json_str() {
    sed -n 's/^ *"'"$1"'": *"\([^"]*\)",\{0,1\} *$/\1/p' "$2" | head -n 1
}

# check_tag verifies a .mac tag file against the file it covers: $1 the file,
# $2 the tag, $3 the key in hex, $4 the target name the key was derived for.
check_tag() {
    [ "$(json_str alg "$2")" = "HMAC-SHA256" ] || return 1
    [ "$(json_str target "$2")" = "$4" ] || return 1
    want=$(json_str mac "$2")
    [ -n "$want" ] || return 1
    got=$(hmac_hex "$3" "$1") || return 1
    [ "$got" = "$want" ]
}

# require_tag authenticates $1 against its tag $2 and names it $3 in a refusal.
# A tag that is present and wrong always stops the restore. A missing tag, or no
# key to check one with, stops it unless --allow-unauthenticated was given: a
# git remote is encrypted to public keys, so the tag is what binds a snapshot to
# the DEK.
require_tag() {
    if [ -z "$mac_key" ] || [ ! -f "$2" ]; then
        [ -n "$allow_unauth" ] || fail "$3 cannot be authenticated (no tag, or no keyring to check one with): restore with exocomp restore, or pass --allow-unauthenticated only if you are certain this snapshot is yours"
        say "warning: $3 is not authenticated; only sha256 digests are checked"
        return 0
    fi
    check_tag "$1" "$2" "$mac_key" "$target_name" \
        || fail "$3 does not authenticate under the manifest subkey for target $target_name: it was not written by this target's keyring"
}

# keyring_generation prints the generation in a keyring.version file, and 1 when
# the file is absent, as the Go reader does.
keyring_generation() {
    if [ -f "$1" ]; then
        sed -n 's/^ *version: *\([0-9][0-9]*\) *$/\1/p' "$1" | head -n 1
    else
        echo 1
    fi
}

# manual_restore_git walks the card's git chain with the checks "exocomp restore"
# makes: clone the backup branch, authenticate the parts manifest, check each
# part against it, decrypt with the recovery identity, unpack only the expected
# regular files, authenticate MANIFEST.json, and refuse a keyring older than the
# snapshot. The archive is zstd or gzip as the snapshot's name says.
manual_restore_git() {
    require_tools git age openssl tar cat cmp find head sort uniq sha256sum awk wc od tr sed grep cut

    repo=$(git_url "$from")

    umask 0077
    work=$(mktemp -d "${TMPDIR:-/tmp}/exocomp-restore.XXXXXX")
    trap 'rm -rf "$work"' EXIT INT TERM

    say "cloning the backup branch of $repo"
    # The checkout runs no filter or hook the remote could name, and nothing the
    # remote prints reaches the terminal unmasked.
    git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c protocol.ext.allow=never \
        -c filter.lfs.smudge= -c filter.lfs.process= -c filter.lfs.required=false \
        clone --quiet --branch backup --depth 1 -- "$repo" "$work/clone" 2> "$work/clone.err" \
        || {
            while IFS= read -r line; do say "git: $line"; done < "$work/clone.err"
            fail "git could not clone the backup branch of $repo"
        }
    clone="$work/clone"

    # Every file is read by name from the top of the tree, and a link there could
    # point a read at a file on this host.
    if [ -n "$(find "$clone" -path "$clone/.git" -prune -o -type l -print | head -n 1)" ]; then
        fail "the backup branch of $repo holds a symbolic link"
    fi

    # The leg force-pushes one snapshot over the branch, so more than one is not
    # a state it writes.
    pm=""
    npm=0
    for f in "$clone"/snapshot-*.age.parts.json; do
        if [ -f "$f" ]; then
            pm="$f"
            npm=$((npm + 1))
        fi
    done
    [ -n "$pm" ] || fail "the backup branch of $repo holds no snapshot-*.age.parts.json"
    [ "$npm" -eq 1 ] || fail "the backup branch of $repo holds $npm snapshots, and the backup leg keeps exactly one"
    snapshot=$(basename "$pm" .parts.json)
    case "$snapshot" in
        *.tar.zst.age) require_tools zstd; unpack="zstd -d -c" ;;
        *.tar.gz.age)  require_tools gzip; unpack="gzip -dc" ;;
        *) fail "$snapshot is neither a .tar.zst nor a .tar.gz snapshot" ;;
    esac

    # The target name keys the tags, so a name read from the branch lets the
    # branch pick which target's tags it passes for.
    [ -n "$target_name" ] || target_name=$(ask "target name the backup was written under: ") || true
    if [ -z "$target_name" ]; then
        target_name=$(sed -n 's/^target-name: *//p' "$clone/RECOVERY.md" 2>/dev/null | head -n 1)
        [ -n "$target_name" ] || fail "target name unknown: pass --target-name"
        say "warning: the target name $target_name comes from the backup branch itself, so it does not show which target this is: pass --target-name"
    fi

    if [ -n "$keyring" ]; then
        [ -r "$keyring" ] || fail "keyring file $keyring is not readable"
        cmp -s "$keyring" "$clone/keyring.age" \
            || fail "keyring.age on the backup branch differs from $keyring: the remote's keyring is not the one you hold"
    else
        say "warning: keyring.age comes from the backup branch itself, so the tags do not show the keyring is yours: pass --keyring with a copy you trust"
    fi

    mac_key=""
    if [ -f "$clone/keyring.age" ] \
        && age -d -i "$identity" "$clone/keyring.age" > "$work/dek.bin" 2>/dev/null; then
        mac_key=$(hkdf_key "$work/dek.bin" "backup-manifest-mac:$target_name" | hex) \
            || fail "openssl could not derive the manifest subkey"
    fi

    require_tag "$pm" "$pm.mac" "$snapshot.parts.json"
    [ "$(json_str snapshot "$pm")" = "$snapshot" ] \
        || fail "$snapshot.parts.json describes a different snapshot than its file name"

    # The parts manifest lists the parts in order, with a digest for each and for
    # the whole. The part files are read by those names, not by a glob.
    awk '
        function val(s) { gsub(/^"/, "", s); gsub(/",?$/, "", s); gsub(/,$/, "", s); return s }
        $1 == "\"parts\":" { inparts = 1; next }
        $1 == "\"size\":"   { if (inparts) psize = val($2); else wsize = val($2) }
        $1 == "\"name\":" && inparts { pname = val($2) }
        $1 == "\"sha256\":" {
            if (inparts) print "part", pname, psize, val($2)
            else wsha = val($2)
        }
        END { print "whole", wsize, wsha }
    ' "$pm" > "$work/parts.list"

    : > "$work/sealed"
    nparts=0
    while read -r kind pname psize psha; do
        [ "$kind" = part ] || continue
        case "$pname" in
            */*) fail "the parts manifest lists $pname, which is not a part of $snapshot" ;;
            "$snapshot".part[0-9]*) ;;
            *) fail "the parts manifest lists $pname, which is not a part of $snapshot" ;;
        esac
        if [ ! -f "$clone/$pname" ] || [ -L "$clone/$pname" ]; then
            fail "part $pname is missing from the backup branch or is not a regular file"
        fi
        [ "$(wc -c < "$clone/$pname" | tr -d ' ')" = "$psize" ] || fail "part $pname is not the size the parts manifest records"
        [ "$(sha256sum "$clone/$pname" | cut -d' ' -f1)" = "$psha" ] || fail "part $pname: sha256 differs from the parts manifest"
        cat "$clone/$pname" >> "$work/sealed"
        nparts=$((nparts + 1))
    done < "$work/parts.list"
    [ "$nparts" -gt 0 ] || fail "$snapshot.parts.json lists no parts"
    wsize=$(awk '$1 == "whole" { print $2 }' "$work/parts.list")
    wsha=$(awk '$1 == "whole" { print $3 }' "$work/parts.list")
    [ "$(wc -c < "$work/sealed" | tr -d ' ')" = "$wsize" ] || fail "the joined parts are not the size the parts manifest records"
    [ "$(sha256sum "$work/sealed" | cut -d' ' -f1)" = "$wsha" ] || fail "the joined parts: sha256 differs from the parts manifest"

    say "decrypting $nparts part(s) with $identity"
    age -d -i "$identity" < "$work/sealed" > "$work/snapshot.tar.cmp" \
        || fail "age could not decrypt the snapshot with $identity"
    $unpack "$work/snapshot.tar.cmp" > "$work/snapshot.tar" \
        || fail "could not decompress the decrypted snapshot"

    # Only regular files, only the names a restore reads, never the archive's own
    # owners, modes or paths.
    tar -tvf "$work/snapshot.tar" | cut -c1 > "$work/types.list" || fail "could not list the snapshot archive"
    tar -tf "$work/snapshot.tar" > "$work/names.list" || fail "could not list the snapshot archive"
    [ "$(wc -l < "$work/types.list")" -eq "$(wc -l < "$work/names.list")" ] \
        || fail "the snapshot archive holds an entry whose name spans lines"
    if [ -n "$(sort "$work/names.list" | uniq -d | head -n 1)" ]; then
        fail "the snapshot archive holds two entries of the same name"
    fi
    if grep -qv '^-$' "$work/types.list"; then
        fail "the snapshot archive holds an entry that is not a regular file"
    fi
    if grep -q '/' "$work/names.list"; then
        fail "the snapshot archive holds an entry with a path"
    fi
    mkdir "$work/tree"
    members=""
    for name in snapshot.db keyring.age keyring.version MANIFEST.json MANIFEST.mac; do
        if grep -qx "$name" "$work/names.list"; then
            members="$members $name"
        fi
    done
    # members are fixed names without spaces, so the split is intended.
    # shellcheck disable=SC2086
    tar -xf "$work/snapshot.tar" -C "$work/tree" --no-same-owner --no-same-permissions -- $members \
        || fail "could not unpack the snapshot archive"
    for name in $members; do
        if [ ! -f "$work/tree/$name" ] || [ -L "$work/tree/$name" ]; then
            fail "$name in the snapshot is not a regular file"
        fi
    done
    [ -f "$work/tree/MANIFEST.json" ] || fail "the restored snapshot holds no MANIFEST.json"

    require_tag "$work/tree/MANIFEST.json" "$work/tree/MANIFEST.mac" "MANIFEST.json"

    # An older keyring wraps the DEK to recipients since revoked.
    want_gen=$(sed -n 's/^ *"keyring_version": *\([0-9][0-9]*\),\{0,1\} *$/\1/p' "$work/tree/MANIFEST.json" | head -n 1)
    if [ -n "$want_gen" ] && [ "$want_gen" -gt 0 ]; then
        for ver in "$clone/keyring.version" "$work/tree/keyring.version"; do
            have_gen=$(keyring_generation "$ver")
            [ -n "$have_gen" ] || fail "cannot read the keyring generation in $ver"
            if [ "$have_gen" -lt "$want_gen" ]; then
                fail "a keyring at generation $have_gen is older than the snapshot's generation $want_gen: fetch the current keyring.age"
            fi
        done
    fi

    install_staged "$work/tree" "$repo"
}

require_tools() {
    for tool in "$@"; do
        command -v "$tool" >/dev/null 2>&1 || fail "$tool is required for a manual restore and is not on PATH"
    done
}

# No container runtime is required to restore: recovery must not depend on
# anything but the files and off-the-shelf tools (AGENTS.md invariant 7).
echo "recovery.sh $VERSION: dest=$dest"
# exocomp restore has no way to pin a keyring, so a pin is only honoured by the
# chain run here. Only a source that is certainly not git, by the rules of
# is_git_target, goes to the binary with --keyring set, and it is told so.
if [ -n "$exocomp_bin" ] && [ -n "$keyring" ] && [ -n "$from" ]; then
    if is_git_target "$from"; then
        echo "recovery.sh: exocomp restore cannot pin a keyring; running the git chain by hand to honour --keyring" >&2
        exocomp_bin=""
    else
        echo "recovery.sh: --keyring applies to a git target only; exocomp restore ignores it" >&2
    fi
fi
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
    if [ -n "$allow_unauth" ]; then
        set -- "$@" --allow-unauthenticated
    fi
    say "exec $exocomp_bin $*"
    "$exocomp_bin" "$@"
fi
