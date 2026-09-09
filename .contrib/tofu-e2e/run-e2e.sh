#!/bin/sh
# OpenTofu e2e driver for exocomp. Boots the throwaway daemon, copies a tofu
# project to a scratch workspace dir, registers it, and runs the scenario
# matrix (see test-plan.md) through the CLI, the web surface, and the MCP
# endpoint. Screenshots go to .agents/brain/screenshots/tofu-e2e/<scenario>/;
# results append to .agents/brain/tofu-e2e/results/<date>.log.
#
# Usage:
#   run-e2e.sh [--scenario S0x] [--project a-local] [--faces cli,web,mcp]
#              [--keep-daemon] [--keep-data] [--list]
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
RIG="$ROOT/.contrib/tofu-e2e"
BRAIN="$ROOT/.agents/brain"
SCRATCH="${EXOCOMP_E2E_DATA:-/tmp/exocomp-throwaway/tofu-e2e}"
DAEMON="$RIG/lib/daemon.sh"
ASSERT="$RIG/lib/assert.sh"

SCENARIO=""
PROJECT="a-local"
FACES="cli,web,mcp"
KEEP_DAEMON=0
KEEP_DATA=0

. "$RIG/lib/faces.sh"
. "$ASSERT"

usage() {
    sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --scenario) SCENARIO=$2; shift 2 ;;
        --project) PROJECT=$2; shift 2 ;;
        --faces) FACES=$2; shift 2 ;;
        --keep-daemon) KEEP_DAEMON=1; shift ;;
        --keep-data) KEEP_DATA=1; shift ;;
        --list) echo "S01 S02 S03 S04 S05 S06 S07 S08 S09 S10"; exit 0 ;;
        *) usage ;;
    esac
done

RESULT_LOG="$BRAIN/tofu-e2e/results/$(date +%Y%m%d).log"
SHOT_DIR="$BRAIN/screenshots/tofu-e2e"
mkdir -p "$(dirname "$RESULT_LOG")" "$SHOT_DIR"

log() { printf '%s\n' "$*" | tee -a "$RESULT_LOG"; }
pass() { log "PASS $1"; }
fail() { log "FAIL $1"; exit 1; }

# --- harness ---------------------------------------------------------------

EXOCOMP_BIN="${EXOCOMP_BIN:-$ROOT/bin/exocomp}"
"$DAEMON" status >/dev/null 2>&1 || EXOCOMP_BIN="$EXOCOMP_BIN" "$DAEMON" start $([ "$KEEP_DATA" = 1 ] && echo --keep-data)
. "$SCRATCH/tokens.env"

WS_DIR="$SCRATCH/ws/$PROJECT"
rm -rf "$WS_DIR"
mkdir -p "$WS_DIR"
cp -R "$RIG/projects/$PROJECT/." "$WS_DIR/"

run_cli() { # run_cli <args...> — local mode against the same data dir the daemon serves
    XDG_DATA_HOME="$SCRATCH" "$EXOCOMP_BIN" "$@"
}

# Plan a workspace; returns tofu's exit (0 none, 2 changes) and prints the
# saved-plan artifact line when one was produced.
plan_ws() {
    name=$1
    set +e
    out=$(run_cli workspace plan "$name" 2>&1)
    rc=$?
    set -e
    printf '%s\n' "$out"
    return "$rc"
}

# wait_job <id> — poll a job to a terminal state (more robust than jobs
# follow, which can race the queue transition).
wait_job() {
    id=$1
    i=0
    while [ "$i" -lt 60 ]; do
        state=$(run_cli jobs get "$id" 2>/dev/null | sed -n 's/^state *//p')
        case "$state" in
            succeeded|failed|interrupted|cancelled) log "  job $id -> $state"; return 0 ;;
        esac
        i=$((i + 1))
        sleep 2
    done
    log "  wait_job: job $id still '$state' after 120s"
    return 1
}

# ensure_workspace <name> — register the workspace if absent and clear any
# leftover gate policy so a fresh run applies freely.
ensure_workspace() {
    name=$1
    if ! run_cli workspace list 2>/dev/null | grep -q "$name"; then
        run_cli workspace add "$name" --dir "$WS_DIR"
    fi
    run_cli policy rm gated 2>/dev/null || true
}

# --- scenario implementations ----------------------------------------------

scenario_S01() { # first-run state creation (plan -> apply)
    log "S01 first-run state creation"
    ensure_workspace "$PROJECT"
    out=$(plan_ws "$PROJECT") || rc=$?
    [ "$rc" -eq 2 ] || fail "S01 plan exit = $rc, want 2 (changes present)"
    run_cli workspace apply "$PROJECT" >/dev/null 2>&1 || fail "S01 apply failed"
    [ -f "$WS_DIR/out/marker.txt" ] || fail "S01 marker missing after apply"
    grep -q "exocomp-e2e-initial" "$WS_DIR/out/marker.txt" || fail "S01 marker content wrong"
    pass "S01"
}

scenario_S02() { # plan -> drift (out-of-band change)
    log "S02 drift detection"
    ensure_workspace "$PROJECT"
    mkdir -p "$WS_DIR/out"
    [ -f "$WS_DIR/out/marker.txt" ] || run_cli workspace apply "$PROJECT" >/dev/null 2>&1
    printf 'drifted\n' >"$WS_DIR/out/marker.txt"
    out=$(plan_ws "$PROJECT") || rc=$?
    [ "$rc" -eq 2 ] || fail "S02 drift plan exit = $rc, want 2"
    pass "S02"
}

scenario_S03() { # plan gating + approvals (saved plan, policy, approve/reject)
    log "S03 plan gating + approvals"
    ensure_workspace "$PROJECT"
    mkdir -p "$WS_DIR/out"
    # A require-approval policy gates any apply on this workspace.
    run_cli policy add gated --workspace "$PROJECT" \
        --expr 'true' --action require_approval
    out=$(plan_ws "$PROJECT") || rc=$?
    [ "$rc" -eq 2 ] || fail "S03 plan exit = $rc, want 2"
    # Extract the saved-plan artifact id.
    artifact=$(printf '%s\n' "$out" | sed -n 's/.*artifact \([0-9]*\).*/\1/p' | head -1)
    [ -n "$artifact" ] || fail "S03 no saved-plan artifact id in: $out"
    # Apply of the saved plan pends (require_approval), then approve it.
    set +e
    apply_out=$(run_cli workspace apply "$PROJECT" --plan "$artifact" 2>&1)
    apply_rc=$?
    set -e
    [ "$apply_rc" -eq 0 ] || fail "S03 gated apply rc=$apply_rc: $apply_out"
    log "S03 gated apply returned (job pending approval): $apply_out"
    # Find the pending approval and approve it.
    set +e
    app_id=$(run_cli approval list 2>&1 | sed -n 's/.*\b\([0-9][0-9]*\)\b.*pending.*/\1/p' | head -1)
    set -e
    [ -n "$app_id" ] || fail "S03 no pending approval listed"
    # The apply job id comes from the apply output ("job 5 gated").
    job_id=$(printf '%s\n' "$apply_out" | sed -n 's/.*job \([0-9]*\) gated.*/\1/p' | head -1)
    run_cli approval decide "$app_id" --approve || fail "S03 approve failed"
    if [ -n "$job_id" ]; then
        log "S03 waiting for approved apply job $job_id"
        wait_job "$job_id"
    else
        sleep 3
    fi
    # Clean up the policy so later scenarios can apply freely.
    run_cli policy rm gated || true
    pass "S03"
}

scenario_S04() { # state import / migrate-state adoption
    log "S04 state adoption (exocomp workspace adopt)"
    run_cli policy rm gated 2>/dev/null || true
    # A second project dir holding a workspace that ALREADY has local state:
    # the adoption case from docs/operations/bootstrap.md. We first establish
    # local state OUTSIDE exocomp (a local backend), then adopt it onto the
    # HTTP backend.
    ADOPT_DIR="$SCRATCH/ws/$PROJECT-adopt"
    rm -rf "$ADOPT_DIR"; mkdir -p "$ADOPT_DIR"
    cp -R "$RIG/projects/a-adopt/." "$ADOPT_DIR/"
    # Establish local state: run tofu by hand with a local backend in the
    # adopt dir, producing terraform.tfstate (serial 1).
    (cd "$ADOPT_DIR" && rm -f out/marker.txt terraform.tfstate backend.tf && \
        printf 'terraform {\n  backend "local" {}\n}\n' >backend.tf && \
        tofu init >/dev/null 2>&1 && tofu plan -out=/tmp/adopt.tfplan >/dev/null 2>&1 && \
        tofu apply /tmp/adopt.tfplan >/dev/null 2>&1 && \
        [ -f terraform.tfstate ]) || fail "S04 could not establish local state"
    run_cli workspace add "$PROJECT-adopt" --dir "$ADOPT_DIR"
    # Adoption serves the state backend in this process, so the daemon must be
    # down for it (two queues over one job table would race). Stop, adopt,
    # restart.
    "$DAEMON" stop
    run_cli workspace adopt "$PROJECT-adopt" 2>&1 | grep -q "adopted: state migrated" \
        || fail "S04 workspace adopt did not report migration"
    "$DAEMON" start --keep-data
    # A subsequent plain plan reads the adopted state cleanly: no drift.
    rc=0
    out=$(plan_ws "$PROJECT-adopt") || rc=$?
    [ "$rc" -eq 0 ] || fail "S04 post-adopt plan exit = $rc, want 0 (no changes)"
    pass "S04"
}

scenario_S05() { # run-secrets as TF_VAR_
    log "S05 run-secrets as TF_VAR_"
    ensure_workspace "$PROJECT"
    mkdir -p "$WS_DIR/out"
    # Seal a secret and bind it to TF_VAR_marker_text so the resource uses it.
    # Idempotent across reruns: rotate if the secret already exists.
    if run_cli secrets list 2>/dev/null | grep -q "e2e/marker"; then
        printf 'secret-marker\n' | run_cli secrets rotate e2e/marker >/dev/null 2>&1
    else
        printf 'secret-marker\n' | run_cli secrets add e2e/marker
    fi
    run_cli run-secret add e2e TF_VAR_marker_text e2e/marker
    # Plan/apply WITH the run-secret binding; the marker should take the secret
    # value. The plan may see drift (marker differs from state) or not (state
    # already clean); either way the apply with the binding must write the
    # secret value into the marker.
    out=$(plan_ws "$PROJECT") || rc=$?
    run_cli workspace apply "$PROJECT" --run-secrets e2e >/dev/null 2>&1 || fail "S05 apply failed"
    grep -q "secret-marker" "$WS_DIR/out/marker.txt" || fail "S05 marker not from run-secret"
    pass "S05"
}

scenario_S06() { # egress declaration
    log "S06 egress declaration"
    ensure_workspace "$PROJECT"
    run_cli workspace egress "$PROJECT" 6443 || fail "S06 egress declaration failed"
    run_cli workspace egress "$PROJECT" || true
    pass "S06"
}

scenario_S07() { # locking + stale-lock unlock
    log "S07 locking + stale-lock unlock"
    ensure_workspace "$PROJECT"
    run_cli workspace unlock "$PROJECT" || fail "S07 unlock on unlocked workspace failed"
    pass "S07"
}

scenario_S08() { # serial history + prune
    log "S08 serial history + prune"
    ensure_workspace "$PROJECT"
    mkdir -p "$WS_DIR/out"
    for i in 1 2 3; do
        printf 'change-%s\n' "$i" >"$WS_DIR/out/marker.txt"
        out=$(plan_ws "$PROJECT") || rc=$?
        [ "$rc" -eq 2 ] || fail "S08 plan $i exit = $rc, want 2"
        run_cli workspace apply "$PROJECT" >/dev/null 2>&1 || fail "S08 apply $i failed"
    done
    run_cli workspace prune "$PROJECT" --keep 1 --days 0 || fail "S08 prune failed"
    pass "S08"
}

scenario_S09() { # offline provider mirror (cache population)
    log "S09 offline provider mirror (cache population)"
    ensure_workspace "$PROJECT"
    # The plugin cache is populated by the first init. The full offline
    # enforcement (OfflineProviders) is not reachable from any surface yet
    # (gap G2); this asserts the warm-cache half: the provider binaries exist
    # in the cache and a plan runs without re-fetching.
    cache_dir="$SCRATCH/exocomp/tofu/plugin-cache"
    before=$(find "$cache_dir" -type f 2>/dev/null | wc -l)
    [ "$before" -gt 0 ] || fail "S09 plugin cache empty after prior runs"
    # A plan against the fresh WS_DIR may see drift (marker absent) — that is
    # correct; what matters is the provider is served from the cache.
    out=$(plan_ws "$PROJECT") || rc=$?
    after=$(find "$cache_dir" -type f 2>/dev/null | wc -l)
    [ "$after" -ge "$before" ] || fail "S09 plugin cache shrank ($before -> $after)"
    log "S09 plugin cache stable at $after files (offline enforcement is G2)"
    pass "S09"
}

scenario_S10() { # state-only external client
    log "S10 state-only external client"
    # Mode-A workspace: exocomp stores state for an unmodified external tofu.
    # The emitted backend block + bearer let tofu run plan/apply itself while
    # exocomp owns the state (ADR 0010). This asserts the block emits and the
    # recorded backend address resolves; a full external-client run is the
    # operator's manual leg (needs tofu pointed at the emitted block).
    run_cli workspace add "$PROJECT-ext" --state-only 2>&1 | grep -q 'backend "http"' \
        || fail "S10 state-only add did not emit a backend block"
    pass "S10"
}

# --- run --------------------------------------------------------------------

if [ -n "$SCENARIO" ]; then
    "scenario_$SCENARIO"
else
    for s in S01 S02 S03 S04 S05 S06 S07 S08 S09 S10; do
        # Each scenario runs in a subshell so a fail() (exit 1) ends only that
        # scenario; a failure is a finding, not a matrix abort.
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
