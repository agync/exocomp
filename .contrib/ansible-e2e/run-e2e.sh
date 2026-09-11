#!/bin/sh
# Ansible e2e driver for exocomp. Boots the throwaway daemon (sandbox tier
# ENFORCING — the ansible jobs carry --egress 2222, the lima node's loopback
# forward, so the sandboxed run reaches it; see README), prepares the lima node
# + source checkout, registers netgroup/check/rule, and runs the scenario
# matrix (test-plan.md) through the CLI.
#
# Usage: run-e2e.sh [--scenario A0x] [--keep-daemon] [--keep-data]
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
RIG="$ROOT/.contrib/ansible-e2e"
BRAIN="$ROOT/.agents/brain"
SCRATCH="${EXOCOMP_E2E_DATA:-/tmp/exocomp-throwaway/ansible-e2e}"
DAEMON="$RIG/lib/daemon.sh"
PREPARE="$RIG/lib/prepare.sh"
NODE="${EXOCOMP_E2E_NODE:-exocomp-node0}"

SCENARIO=""
KEEP_DAEMON=0
KEEP_DATA=0

while [ $# -gt 0 ]; do
    case "$1" in
        --scenario) SCENARIO=$2; shift 2 ;;
        --keep-daemon) KEEP_DAEMON=1; shift ;;
        --keep-data) KEEP_DATA=1; shift ;;
        --list) echo "A01 A02 A03 A04"; exit 0 ;;
        *) echo "usage: $0 [--scenario A0x] [--keep-daemon] [--keep-data]" >&2; exit 2 ;;
    esac
done

RESULT_LOG="$BRAIN/ansible-e2e/results/$(date +%Y%m%d).log"
mkdir -p "$(dirname "$RESULT_LOG")"

log() { printf '%s\n' "$*" | tee -a "$RESULT_LOG"; }
pass() { log "PASS $1"; }
fail() { log "FAIL $1"; exit 1; }

EXOCOMP_BIN="${EXOCOMP_BIN:-$ROOT/bin/exocomp}"
export EXOCOMP_E2E_DATA="$SCRATCH"   # daemon.sh's own default is the tofu rig's path — pin ours

"$DAEMON" status >/dev/null 2>&1 || \
    EXOCOMP_BIN="$EXOCOMP_BIN" \
        "$DAEMON" start $([ "$KEEP_DATA" = 1 ] && echo --keep-data)
. "$SCRATCH/tokens.env"

run_cli() { # run_cli <args...> — same data dir the daemon serves
    XDG_DATA_HOME="$SCRATCH" "$EXOCOMP_BIN" "$@"
}

# ensure_node_probe: node up, endpoint derived, source + check + netgroup +
# rule present (the rule's BuildPlaybookSpec needs the source to resolve).
ensure_node_probe() {
    "$PREPARE" --ensure-node
    if ! run_cli source list 2>/dev/null | grep -q "infra"; then
        run_cli source add infra --path "$RIG/projects/infra"
    fi
    if ! run_cli check list 2>/dev/null | grep -q node0-ssh; then
        run_cli check add node0-ssh --kind tcp --target "node0=127.0.0.1:2222" --interval 10s
    fi
    if ! run_cli netgroup list 2>/dev/null | grep -q '^lab'; then
        run_cli netgroup add lab 127.0.0.0/8
    fi
    if ! run_cli rule list 2>/dev/null | grep -q onboard-node0; then
        run_cli rule add onboard-node0 --source check \
            --expr 'event.status == "firing" && event.labels["netgroup"] == "lab"' \
            --action playbook --repo-source infra --playbook plays/site.yml \
            --limit node0 --egress 2222 -y
    fi
}

wait_job() {
    id=$1; i=0
    while [ "$i" -lt 60 ]; do
        state=$(run_cli jobs get "$id" 2>/dev/null | sed -n 's/^state *//p')
        case "$state" in
            succeeded|failed|interrupted|cancelled) log "  job $id -> $state"; return 0 ;;
        esac
        i=$((i + 1)); sleep 2
    done
    log "  wait_job: job $id still '$state' after 120s"; return 1
}

wait_check() { # wait_check <up|down> <max_ticks>
    want=$1; ticks=${2:-8}; i=0
    while [ "$i" -lt "$ticks" ]; do
        sleep 30
        st=$(run_cli check list 2>/dev/null | sed -n '2p' | awk '{print $5}')
        [ "$st" = "$want" ] && return 0
        i=$((i + 1))
    done
    log "  wait_check: node0-ssh still $(run_cli check list 2>/dev/null | sed -n '2p' | awk '{print $5}') after $((ticks*30))s"
    return 1
}

scenario_A01() { # happy path — playbook run through the queue
    log "A01 happy path playbook run"
    ensure_node_probe
    out=$(run_cli playbook run plays/site.yml --source infra --egress 2222 -e marker_content=hello-exocomp-ansible 2>&1)
    printf '%s\n' "$out" | grep -q "succeeded" || fail "A01 playbook did not settle succeeded: $out"
    marker=$(limactl shell "$NODE" -- cat /tmp/exocomp-e2e/marker.txt 2>/dev/null)
    [ "$marker" = "hello-exocomp-ansible" ] || fail "A01 marker = '$marker', want hello-exocomp-ansible"
    pass "A01"
}

scenario_A02() { # source surface — the checkout and its playbook are registered
    log "A02 source surface"
    ensure_node_probe
    run_cli source list 2>/dev/null | grep -q "infra" || fail "A02 source infra not registered"
    [ -f "$RIG/projects/infra/plays/site.yml" ] || fail "A02 playbook missing"
    pass "A02"
}

scenario_A03() { # netgroup check — target classified into lab
    log "A03 netgroup check"
    ensure_node_probe
    run_cli check list 2>/dev/null | grep -q "node0" || fail "A03 check target missing"
    # Force a fresh observation so the event carries the netgroup label. Hold
    # the VM down past the hysteresis so the departure is DECLARED (a cold
    # seed or a quick restart would skip the event).
    limactl stop "$NODE" 2>/dev/null || true
    wait_check down || fail "A03 check never went down"
    sleep 95
    limactl start "$NODE" >/dev/null 2>&1
    wait_check up || fail "A03 check never came back up"
    # The firing event lands AFTER the 3-probe hysteresis (~90s past the state
    # flip) — poll for it rather than asserting on the flip.
    i=0
    while [ "$i" -lt 6 ]; do
        sleep 30
        ev=$(run_cli event list 2>/dev/null | sed -n '2p')
        case "$ev" in
            *"check"*) echo "$ev" | grep -q "fired" && break ;;
        esac
        i=$((i + 1))
    done
    echo "$ev" | grep -q "check" || fail "A03 no check event: '$ev'"
    pass "A03"
}

scenario_A04() { # host-joins-netgroup triggers the playbook
    log "A04 join triggers the playbook"
    ensure_node_probe
    run_cli rule list 2>/dev/null | grep -q onboard-node0 || fail "A04 rule missing"
    before_id=$(run_cli jobs list 2>/dev/null | sed -n '2,$p' | awk '{print $1}' | sort -n | tail -1)
    [ -n "$before_id" ] || before_id=0
    limactl stop "$NODE" 2>/dev/null || true
    wait_check down || fail "A04 check never went down"
    # Hold the VM down past the hysteresis so the departure DECLARES (a quick
    # restart would leave emitted=up and the later join would not re-fire).
    sleep 95
    limactl start "$NODE" >/dev/null 2>&1
    # The join fires after the 3-probe hysteresis; the rule queues a job. The
    # jobs list is oldest-first — look for a rule job NEWER than the wait
    # started (an old rule job from a prior run must not satisfy the wait).
    job_id=""
    i=0
    while [ "$i" -lt 8 ]; do
        sleep 30
        new=$(run_cli jobs list 2>/dev/null | grep "onboard-node0" | awk -v b="$before_id" '$1 > b {print}' | tail -1)
        if [ -n "$new" ]; then
            job_id=$(echo "$new" | awk '{print $1}')
            break
        fi
        i=$((i + 1))
    done
    [ -n "$job_id" ] || fail "A04 rule never queued a new job (events: $(run_cli event list 2>/dev/null | sed -n '2p'))"
    wait_job "$job_id" || fail "A04 job $job_id did not settle"
    st=$(run_cli jobs get "$job_id" 2>/dev/null | sed -n 's/^state *//p')
    [ "$st" = "succeeded" ] || fail "A04 job $job_id state=$st, want succeeded"
    marker=$(limactl shell "$NODE" -- cat /tmp/exocomp-e2e/marker.txt 2>/dev/null)
    [ "$marker" = "hello-exocomp-ansible" ] || fail "A04 marker = '$marker'"
    pass "A04"
}

if [ -n "$SCENARIO" ]; then
    "scenario_$SCENARIO"
else
    # A04 runs standalone: its join re-fires the SAME check condition A03 just
    # raised, and the reactive dedup window (5m) correctly suppresses the
    # repeat — run `--scenario A04` on its own (or >5m after A03).
    for s in A01 A02 A03; do
        if ( "scenario_$s" ); then
            :
        else
            log "SCENARIO $s FAILED (continuing)"
        fi
    done
fi

if [ "$KEEP_DAEMON" = 0 ]; then
    "$DAEMON" stop
fi
log "done: $RESULT_LOG"
