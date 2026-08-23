# Mutation-test the guard

A passing test proves the code works today. It does not prove the test would
notice if the guard were removed. The mutation check closes that gap: revert the
guard to the behaviour it replaced and confirm the regression test fails loudly.
If the test still passes, it never tested the guard — it is decoration, and the
exploit can reopen unnoticed on any future edit.

The `mutation-test-a-guard` skill carries the general procedure; this reference
pins it to the four dispositions this loop produced.

## Procedure

1. Locate the guard the fix added (the `file:line` from the finding's repro).
2. Patch it back to the pre-fix behaviour — the smallest edit that restores the
   hole: flip the fail-closed branch to fall through, drop the scope re-check,
   widen the idempotency namespace back to global, remove the owner comparison.
3. Run only the finding's regression test. It MUST fail, and fail on the
   assertion that names the attack — not on an unrelated panic.
4. Revert the mutation. The guard and its killing test are now paired.

A mutation that leaves the test green means one of: the test asserts a
side-effect the mutation does not touch, the guard is duplicated elsewhere (find
the real one), or the test exercises a different path than the exploit. Fix the
test until the mutation kills it.

## Worked mutations (from the closed wave)

- **Scopes disjoint (AI-2).** Guard: `operator` and `approve` are disjoint, and
  both decision paths refuse the caller the gated job's ledger records as its
  actor. Mutation: let `operator` satisfy the decision route, or drop the
  self-approval refusal. `TestMCPScopesStayDisjoint`
  (`internal/web/mcp_approve_test.go`), `TestAPIDecisionRefusesOperatorScope`
  and `TestDecisionRefusesSelfApproval` (`internal/web/api_decision_scope_test.go`)
  must go red.
- **Plan-policy fail-closed (AI-3).** Guard: `Service.PlanGateForPlan`
  (`internal/service/planseal.go`) returns `PlanUnadjudicatedError` (which
  unwraps to `store.ErrStateConflict`) when a policy-bearing workspace has no
  adjudication record. Mutation: return an empty allow-gate on the missing
  record. `TestPlanGateForPlanFailsClosed`
  (`internal/service/planadjudication_test.go`) and
  `TestAPIApplyRefusesUnadjudicatedPlan` (`internal/web/api_plan_gate_test.go`)
  must go red — and `TestPlanGateForPlanAdmitsUnmatchedPlan` must STAY green, so
  the mutation is a hole, not a blanket refusal.
- **MCP self-approval dropped (AI-1/AI-2).** Guard: the stdio face advertises no
  decision tool, and the HTTP face refuses a caller deciding a job it asked for
  (`internal/web/mcp_http.go`, `internal/mcp/decisions.go`). Mutation: re-add
  `approve_job`/`reject_job` to the write column, or skip the actor check.
  `TestMCPScopesStayDisjoint` must go red.
- **Health-probe TTL (AI-6).** Guard: `GET /api/v1/health` is served from a
  cache behind `healthCacheTTL` (10s, `internal/web/api.go`) with a serialized
  refresh. Mutation: probe per request. `TestAPIHealthProbeIsCached`
  (`internal/web/api_health_cache_test.go`) must go red on the probe-count
  assertion (a read flood must cost one probe, not one per read).
