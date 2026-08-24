# Worked examples — the shape of a well-closed finding

Four dispositions from the closed `AI-*` wave, each showing the loop end to end:
the hole, the fail-closed guard, the test that kills it, and the residual the
lane could not close structurally. They are here as a template for the shape of
a good finding, not for their detail — read `docs/security/threat-model.md` for
the authoritative record.

## Scopes stay disjoint (AI-2 · disposition FIX)

- **Hole.** `operator` satisfied both the write column (enqueue a gated apply)
  and the approve column (settle its approval). Handing an agent the token it
  needs to plan/apply handed it the authority the gate exists to withhold.
- **Guard.** The `/api/v1` decision route takes `approve` alone; `operator` and
  `approve` are disjoint scopes; and BOTH decision paths refuse a caller the
  gated job's ledger records as its actor. The property is structural (scope
  membership), not a rule a handler remembers.
- **Test.** `TestMCPScopesStayDisjoint`, `TestAPIDecisionRefusesOperatorScope`,
  `TestDecisionRefusesSelfApproval`.
- **Residual.** The self-approval refusal keys on the token NAME recorded in the
  ledger, so a revoked name later reused would read as the same principal.

## Plan-policy fails closed (AI-3 · disposition FIX)

- **Hole.** A TOCTOU window between `CreateArtifact` and the verdict event: an
  apply-by-id landing in the gap read "no verdict" as "no policy" and enqueued
  plainly against a deny-all workspace.
- **Guard.** A plan on a workspace with enabled policies and no adjudication
  record is unconsumable on every face (`PlanGateForPlan` →
  `PlanUnadjudicatedError`, unwrapping to `store.ErrStateConflict`); the seal
  records a no-match adjudication so a legitimately unmatched plan still applies;
  and policy is evaluated BEFORE the seal, shrinking the window to one event
  append.
- **Test.** `TestPlanGateForPlanFailsClosed` plus
  `TestPlanGateForPlanAdmitsUnmatchedPlan` (the fail-closed rule must not refuse
  a plan the policies legitimately did not match).
- **Residual.** Seal and verdict are still two writes, not one transaction; a
  store-level transaction spanning both would close the window structurally, but
  that needs the store seam the lane does not own.

## MCP self-approval dropped (AI-1/AI-2 · disposition FIX)

- **Hole.** MCP stdio had no credential and no scope, yet advertised
  `approve_job`/`reject_job` beside `workspace_apply` — an agent could gate its
  own destructive job and then approve it, and prompt-injected webhook text
  reached the same session that held the release.
- **Guard.** The decision tools are gone from the stdio face; a gated job is
  decided from the browser/phone surface, the CLI, or an `approve`-scope
  `/api/v1` token. They return only with a token-carrying HTTP/SSE transport
  under ADR 0014 §3's scope-gated visibility.
- **Test.** `TestMCPScopesStayDisjoint` on the MCP HTTP face.
- **Residual.** The stdio face still runs unauthenticated at the local
  operator's authority for `workspace_plan`/`workspace_apply` — a stated
  deferral, bounded by the fact it can no longer release its own gate.

## Health-probe TTL (AI-6 · disposition FIX)

- **Hole.** `GET /api/v1/health` let a READONLY token fork eight PATH-resolved
  subprocesses and unwrap the keyring per request, at any rate — outside the job
  queue and the KDF gate.
- **Guard.** The summary is served from a short cache (`healthCacheTTL`, 10s)
  whose refresh is serialized, so a read flood costs one probe and concurrent
  readers share it.
- **Test.** `TestAPIHealthProbeIsCached` (a burst of reads runs exactly one
  probe; past the TTL the next read refreshes).
- **Residual.** The probe still resolves optional tools by bare name on the
  ambient PATH and calls `secrets.Open` per run to mint the scrape token;
  pinning tool paths and deriving that token once at daemon start belong to
  `internal/health`, not this lane.

## Layered guard mutation (illustrative)

Not a specific closed finding — a generic shape for mutation-check.md's
"Layered guards" case, concrete enough to apply.

- **Hole.** An exploit that TWO independent guards each separately refuse —
  e.g. a scope check and an actor check both stand between a caller and a
  gated release.
- **Guard.** Reverting only the nearer guard (say, the actor check) leaves the
  farther guard (scope disjointness) still blocking the same attack path, so
  a naive mutation test stays green — a false pass that looks like proof but
  isn't.
- **Test.** Mutate BOTH guards together (the true pre-fix state), or assert on
  the guard-specific observable each layer alone controls (the actor check's
  own error, not just "request refused") so each layer's mutation trips a
  distinct, layer-specific assertion.
- **Residual.** A single end-to-end assertion ("request was refused") can
  never tell you which layer did the work; only a full pre-fix revert or
  observable-per-layer assertions can.

## What each example shares

A real hole with a reproduced exploit; a guard whose default is *refuse*; a test
that encodes the attack and dies if the guard is reverted; and an honest
residual naming what stayed open and which file owns it. A finding without all
four is not closed — it is described.
