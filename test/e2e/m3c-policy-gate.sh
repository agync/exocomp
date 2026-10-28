#!/bin/sh
# M3c plan-policy gate: a CEL policy over a workspace's `tofu show -json` plan.
# A deny policy that matches a delete-bearing plan seals it unconsumable, so a
# later apply of that plan REFUSES naming the policy; a require_approval policy
# routes the apply through the existing approval gate; a plan nothing matches
# applies clean; and `policy add` refuses a CEL expression that will not
# compile. Runs the real tofu path through one serve daemon, like m2-gate.sh.
set -eu

ROOT=$(git rev-parse --show-toplevel)
WORK=$(mktemp -d)
SERVE_PID=""
cleanup() {
    [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export PATH="$HOME/.local/bin:$PATH"
export XDG_DATA_HOME="$WORK/data"
# Hermetic client env: a hosts.toml default remote, an inherited sandbox
# tier, or an inherited output format would all change what these commands do.
export XDG_CONFIG_HOME="$WORK/config"
export EXOCOMP_REMOTE=
export EXOCOMP_SANDBOX=
export EXOCOMP_OUTPUT=
DATADIR="$XDG_DATA_HOME/exocomp"

command -v tofu >/dev/null 2>&1 || { echo "SKIP: tofu absent"; exit 0; }
command -v nono >/dev/null 2>&1 || { echo "SKIP: nono absent"; exit 0; }

log "build + init"
make -C "$ROOT" -s build
EXO="$ROOT/bin/exocomp"
"$EXO" init >"$WORK/kit.txt"

log "start the daemon"
"$EXO" serve --drain-grace 15s >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 50); do
    grep -q 'state backend listening' "$WORK/serve.log" 2>/dev/null && break
    sleep 0.2
done
ADDR=$(sed -n 's/.*state backend listening on //p' "$WORK/serve.log" | head -1)
[ -n "$ADDR" ] || { cat "$WORK/serve.log"; fail "daemon did not start"; }

# --- a workspace with a resource we will later delete ---
mkdir -p "$WORK/tf"
cat >"$WORK/tf/main.tf" <<'EOF'
resource "local_file" "gate" {
  filename = "${path.module}/gate.txt"
  content  = "hello"
}
EOF
"$EXO" workspace add homelab --dir "$WORK/tf" >/dev/null

log "policy add refuses an expression that will not compile"
if "$EXO" policy add bad --workspace homelab --action deny \
    --expr 'plan.resource_changes.exists(c,' >"$WORK/badpol.out" 2>&1; then
    fail "policy add accepted invalid CEL"
fi
if "$EXO" policy add bad2 --workspace homelab --action deny \
    --expr 'plan.resource_changes' >"$WORK/badpol2.out" 2>&1; then
    fail "policy add accepted a non-bool expression"
fi
"$EXO" policy list 2>&1 | grep -q 'no plan policies' || fail "a rejected policy was stored"

log "add a deny policy on delete-bearing plans"
"$EXO" policy add no-deletes --workspace homelab --action deny \
    --expr 'plan.resource_changes.exists(c, c.change.actions.exists(a, a == "delete"))' \
    >"$WORK/addpol.out" 2>&1 || { cat "$WORK/addpol.out"; fail "policy add failed"; }

log "a create-only plan does not match the deny; it applies clean"
set +e
"$EXO" workspace plan homelab >"$WORK/plan1.out" 2>&1; PLAN1_RC=$?
set -e
[ "$PLAN1_RC" -eq 2 ] || { cat "$WORK/plan1.out"; fail "create plan expected exit 2, got $PLAN1_RC"; }
grep -q 'plan policy: deny' "$WORK/plan1.out" && fail "a create-only plan was denied"
PLAN1_ID=$(sed -n 's/.*saved plan artifact \([0-9]*\).*/\1/p' "$WORK/plan1.out" | head -1)
[ -n "$PLAN1_ID" ] || { cat "$WORK/plan1.out"; fail "no plan artifact id"; }
"$EXO" workspace apply homelab --plan "$PLAN1_ID" >"$WORK/apply1.out" 2>&1 \
    || { cat "$WORK/apply1.out"; fail "clean apply failed"; }
grep -q hello "$WORK/tf/gate.txt" || fail "clean apply did not write the resource"

log "remove the resource so the next plan is a delete"
: >"$WORK/tf/main.tf"
set +e
"$EXO" workspace plan homelab >"$WORK/plan2.out" 2>&1; PLAN2_RC=$?
set -e
[ "$PLAN2_RC" -eq 2 ] || { cat "$WORK/plan2.out"; fail "delete plan expected exit 2, got $PLAN2_RC"; }
grep -q 'plan policy: deny by "no-deletes"' "$WORK/plan2.out" \
    || { cat "$WORK/plan2.out"; fail "the delete plan was not denied at seal"; }
grep -q 'unconsumable' "$WORK/plan2.out" || fail "seal did not warn the plan is unconsumable"
PLAN2_ID=$(sed -n 's/.*saved plan artifact \([0-9]*\).*/\1/p' "$WORK/plan2.out" | head -1)
[ -n "$PLAN2_ID" ] || fail "no delete-plan artifact id"

log "applying the denied plan REFUSES, naming the policy"
if "$EXO" workspace apply homelab --plan "$PLAN2_ID" >"$WORK/apply2.out" 2>&1; then
    cat "$WORK/apply2.out"; fail "apply of a denied plan did not refuse"
fi
grep -q 'no-deletes' "$WORK/apply2.out" || { cat "$WORK/apply2.out"; fail "refusal did not name the policy"; }
grep -q 'unconsumable' "$WORK/apply2.out" || fail "refusal did not call the plan unconsumable"

# --- require_approval routing on a second workspace ---
log "a require_approval policy routes the apply through the gate"
mkdir -p "$WORK/tf2"
cat >"$WORK/tf2/main.tf" <<'EOF'
resource "local_file" "gate2" {
  filename = "${path.module}/gate2.txt"
  content  = "world"
}
EOF
"$EXO" workspace add staging --dir "$WORK/tf2" >/dev/null
"$EXO" policy add gate-changes --workspace staging --action require_approval \
    --expr 'size(plan.resource_changes) > 0' >/dev/null
set +e
"$EXO" workspace plan staging >"$WORK/plan3.out" 2>&1; PLAN3_RC=$?
set -e
[ "$PLAN3_RC" -eq 2 ] || { cat "$WORK/plan3.out"; fail "staging plan expected exit 2, got $PLAN3_RC"; }
grep -q 'plan policy: require_approval by "gate-changes"' "$WORK/plan3.out" \
    || { cat "$WORK/plan3.out"; fail "staging plan verdict not recorded"; }
PLAN3_ID=$(sed -n 's/.*saved plan artifact \([0-9]*\).*/\1/p' "$WORK/plan3.out" | head -1)
"$EXO" workspace apply staging --plan "$PLAN3_ID" >"$WORK/apply3.out" 2>&1 \
    || { cat "$WORK/apply3.out"; fail "gated apply returned an error"; }
grep -q 'requires approval' "$WORK/apply3.out" || fail "gated apply did not report the gate"
grep -q 'awaiting_approval' "$WORK/apply3.out" || fail "gated apply did not gate the job"
# `approval list` shows the approval's own state (pending) beside the gated
# job's runbook; the job itself is awaiting_approval.
"$EXO" approval list >"$WORK/approvals.out" 2>&1
grep -q 'pending' "$WORK/approvals.out" && grep -q 'workspace:staging' "$WORK/approvals.out" \
    || { cat "$WORK/approvals.out"; fail "no pending approval was created for the gated apply"; }

# gate2.txt must NOT exist: the gated apply must not have run.
[ -e "$WORK/tf2/gate2.txt" ] && fail "the gated apply ran without approval" || true

log "ALL PLAN-POLICY GATE CHECKS PASSED"
