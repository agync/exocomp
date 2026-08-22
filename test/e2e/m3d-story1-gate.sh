#!/bin/sh
# M3d story-1 gate: "here is my git repo, go run my playbook regularly."
# A git source pins to a SIGNED ref whose trust is exocomp-OWNED (an
# allowed-signers list carried on the source, not the host's git config):
#   1. `source sync` verifies the pinned tag's SSH signature against exocomp's
#      own allowed-signers, clones, and pins to the signed commit.
#   2. a play runs from that synced checkout.
#   3. a ref signed by an UNTRUSTED key is REFUSED — the bad commit cannot reach
#      the fleet — and so is an unsigned ref.
#   4. the play is scheduled to run regularly.
# Hermetic: a local git "forge" + a localhost play, no lima node needed.
set -eu

ROOT=$(git rev-parse --show-toplevel)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

command -v ansible-playbook >/dev/null 2>&1 || { echo "SKIP: ansible-playbook absent"; exit 0; }
command -v ssh-keygen >/dev/null 2>&1 || { echo "SKIP: ssh-keygen absent"; exit 0; }
command -v git >/dev/null 2>&1 || { echo "SKIP: git absent"; exit 0; }

export XDG_DATA_HOME="$WORK/data"

log "build + init"
make -C "$ROOT" -s build
EXO="$ROOT/bin/exocomp"
"$EXO" init >"$WORK/kit.txt"

log "signing keys: one trusted, one attacker"
ssh-keygen -t ed25519 -N '' -C signer@exocomp -f "$WORK/trusted" -q
ssh-keygen -t ed25519 -N '' -C attacker@exocomp -f "$WORK/attacker" -q
TRUSTED_LINE="signer@exocomp $(awk '{print $1" "$2}' "$WORK/trusted.pub")"

log "a git forge with a good, trusted-signed tag v1"
FORGE="$WORK/forge"
mkdir -p "$FORGE"
git -C "$FORGE" init -q -b main
git -C "$FORGE" config user.email signer@exocomp
git -C "$FORGE" config user.name signer
git -C "$FORGE" config gpg.format ssh
git -C "$FORGE" config user.signingkey "$WORK/trusted.pub"
cat >"$FORGE/site.yml" <<'YML'
- hosts: localhost
  connection: local
  gather_facts: false
  tasks:
    - debug: { msg: "exocomp-story1-ran" }
YML
git -C "$FORGE" add site.yml
git -C "$FORGE" commit -qm "initial playbook"
git -C "$FORGE" tag -s -m "release v1" v1
GOOD_SHA=$(git -C "$FORGE" rev-parse 'v1^{commit}')

log "register the source with exocomp-owned trust, then sync the signed ref"
"$EXO" source add infra --url "file://$FORGE" --ref v1 --signed --signer "$TRUSTED_LINE" >"$WORK/add.out"
"$EXO" source sync infra >"$WORK/sync.out" 2>&1 || { cat "$WORK/sync.out"; fail "signed sync of a trusted ref was refused"; }
grep -q "$(printf %.12s "$GOOD_SHA")" "$WORK/sync.out" || { cat "$WORK/sync.out"; fail "sync did not pin to the signed commit"; }
log "   -> pinned to $(printf %.12s "$GOOD_SHA")"

log "run the play from the synced checkout"
"$EXO" playbook run site.yml --source infra >"$WORK/play.out" 2>&1 || { cat "$WORK/play.out"; fail "play from source did not run"; }
grep -q 'exocomp-story1-ran' "$WORK/play.out" || { cat "$WORK/play.out"; fail "play task did not run"; }
log "   -> play ran from the pinned checkout"

log "an ATTACKER-signed ref v2 is refused (bad commit cannot reach the fleet)"
git -C "$FORGE" config user.email attacker@exocomp
git -C "$FORGE" config user.signingkey "$WORK/attacker.pub"
echo "backdoor" >>"$FORGE/site.yml"
git -C "$FORGE" commit -qam "attacker commit"
git -C "$FORGE" tag -s -m "evil" v2
"$EXO" source add evil --url "file://$FORGE" --ref v2 --signed --signer "$TRUSTED_LINE" >/dev/null
if "$EXO" source sync evil >"$WORK/evil.out" 2>&1; then
    cat "$WORK/evil.out"; fail "attacker-signed ref v2 was accepted"
fi
grep -qi 'verifiable signature' "$WORK/evil.out" || { cat "$WORK/evil.out"; fail "refusal did not name the missing signature"; }
log "   -> attacker-signed ref refused"

log "an UNSIGNED ref is refused"
git -C "$FORGE" tag v3-unsigned
"$EXO" source add plain --url "file://$FORGE" --ref v3-unsigned --signed --signer "$TRUSTED_LINE" >/dev/null
if "$EXO" source sync plain >"$WORK/plain.out" 2>&1; then
    cat "$WORK/plain.out"; fail "unsigned ref v3 was accepted"
fi
log "   -> unsigned ref refused"

log "schedule the play to run regularly"
"$EXO" schedule add nightly-converge --kind shell --cron '@every 24h' -- \
    "$EXO" playbook run site.yml --source infra >/dev/null
"$EXO" schedule list 2>/dev/null | grep -q nightly-converge || fail "schedule was not registered"
log "   -> scheduled"

printf 'PASS: story-1 gate — signed git source with exocomp-owned trust; trusted ref syncs+pins+runs, attacker and unsigned refs refused, play scheduled\n'
