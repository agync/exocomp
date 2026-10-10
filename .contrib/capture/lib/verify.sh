#!/bin/sh
# State assertions for the capture rig. A render is only worth capturing if the
# state behind it is the state the render claims, so the rig asserts before it
# shoots and fails loudly rather than silently producing a wrong picture.
#
# The list verbs are read through --output json. Every list envelope is
# {"schema_version","kind","data":[...]}, so the parsers here only ever touch
# data[] and are indifferent to the rest of the envelope.
set -eu

fail() { die "verify: $1: want [$2], got [$3]"; }

_json() { # _json <json> <python-statements over d>
    # The caller passes statements and prints its own result, so this only
    # prepends the preamble. Wrapping in print() would turn a statement block
    # into a syntax error, which is what this used to do.
    # Stderr is surfaced rather than swallowed: a silent parse failure reads as
    # "the state is wrong" when the real fault is a broken expression.
    out=$(printf '%s' "$1" | python3 -c "
import json,sys
d=json.load(sys.stdin)
$2
" 2>&1) || die "verify: python failed: $out"
    printf '%s' "$out"
}

# assert_check <name> <target-count> <wanted-state>
#   wanted-state is the per-target status joined by commas. A target the probe
#   subsystem has never produced a result for carries no status, which reads as
#   unobserved.
assert_check() {
    name=$1; targets=$2; want=$3
    json=$(cli check list --output json 2>/dev/null) || json=""
    got=$(_json "$json" "
rows=d.get('data',[])
c=[r for r in rows if r.get('name')=='$name']
if not c: print('MISSING'); raise SystemExit
ts=c[0].get('targets') or []
st=[t.get('status') or 'unobserved' for t in ts]
while len(st) < $targets: st.append('unobserved')
print(','.join(st[:$targets]))
")
    [ "$got" = "$want" ] || fail "check $name" "$want" "$got"
    log "  ok check $name: $got"
}

# assert_secret <name>: a named secret exists. Counting is deliberately avoided:
# `source add` seals a deploy key of its own, so the row count is not the number
# of secrets a person added.
assert_secret() {
    name=$1
    json=$(cli secrets list --output json 2>/dev/null) || json=""
    got=$(_json "$json" "
rows=d.get('data',[])
print('present' if any(r.get('name')=='$name' for r in rows) else 'MISSING')
")
    [ "$got" = "present" ] || fail "secret $name" "present" "$got"
    log "  ok secret $name present"
}

# assert_source_field_absent <name> <field>: a source that has never synced
# carries no sync timestamp at all, which is the honest assertion.
assert_source_field_absent() {
    name=$1; field=$2
    json=$(cli source list --output json 2>/dev/null) || json=""
    got=$(_json "$json" "
rows=d.get('data',[])
c=[r for r in rows if r.get('name')=='$name']
if not c: print('MISSING'); raise SystemExit
print('absent' if not c[0].get('$field') else 'present:'+str(c[0].get('$field')))
")
    [ "$got" = "absent" ] || fail "source $name $field" "absent" "$got"
    log "  ok source $name has no $field"
}

verify_renders() {
    log "verifying render state before capture"
    # Two states the probe subsystem produces reliably from a fresh seed: the
    # unreachable targets settle down on their own, with no managed node.
    assert_check presence 3 "down,down,down"
    assert_check web-health 1 "down"
    # One the rig declares but never lets the probe reach.
    assert_source_field_absent site last_sync
    assert_secret restic-repo
    assert_secret ntfy-token
    log "state verified"
}

# wait_until_verified <seconds>: probe results land on the sweep's own clock, so
# a fixed sleep is either too short or wasteful. Poll the assertion instead and
# fail with the last observed state rather than a guessed one.
wait_until_verified() {
    budget=${1:-60}
    i=0
    while [ "$i" -lt "$budget" ]; do
        if msg=$(verify_renders 2>&1); then
            printf '%s\n' "$msg" >&2
            return 0
        fi
        sleep 5
        i=$((i + 5))
        printf '  ... %ss waiting for the probe sweep\n' "$i" >&2
    done
    die "state never settled after ${budget}s; last failure: $msg"
}
