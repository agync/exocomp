# Code review 2026-08-24 — Info-level triage

Disposition of the Info-level findings from a full-codebase review. The Moderate, Minor,
Simplification, and Non-functional findings were fixed and landed; this note records the
Info-level items — each promoted to a fix, verified safe, or accepted with rationale.

## Promoted to fixes during remediation
- **`/kill` route never registered.** Fixed — `POST /kill` now drives an operator-gated
  global silence (drives the PAUSED banner). See the log entry of the same date.
- **`EventByJobID` scans without a `job_id` index.** Fixed — partial index added via
  migration 0025; `EXPLAIN QUERY PLAN` confirms the index is used with no table scan.

## Verified reachable but not a bug (regression-pinned)
- **`ReconcileNonTerminal` may retire a legitimately-queued job whose enqueuer exited.**
  Confirmed reachable: ephemeral CLI commands enqueue via `queue.New(st, enqueueOnly{}, …)`
  and exit, stamping their own pid as `owner_pid`; `ClaimJob` ignores `owner_pid`, so the
  daemon can still claim the row, but if the sweep runs first the row is retired to
  `interrupted`. This is intentional, not data loss: composed workflow chains self-heal —
  the daemon's `driveWorkflows`/`ResumeAll` loop re-arms an interrupted, ledger-unfinished
  step under a fresh job with the daemon's own live pid within one tick; and for standalone
  jobs, "`interrupted` is terminal and never auto-retried" is the documented contract. In
  practice the 200 ms claim poll beats the 30 s sweep except under genuine contention. A
  comment at `reconcileOwned` and the regression test
  `TestSequencerRearmsStepInterruptedWhileQueued` (mutation-checked) pin this.

## Accepted (documented, not code)
- **Browser decide path relies on `SameSite=Strict` alone; no Origin/Referer check.**
  ACCEPT. The web surface is single-operator and loopback / tailnet-scoped (never
  `tailscale funnel`); `SameSite=Strict` on the session cookie is an adequate CSRF defense,
  and the new `/kill` handler deliberately mirrors this posture. An explicit Origin/Referer
  check is optional future hardening, not required by the threat model.
- **MCP resource failures reported as generic RPC `internal_error`.** ACCEPT for now. Agents
  read prose rather than a structured code — a DX nicety, not a correctness or security
  issue. Candidate small follow-up: map resource failures to distinct MCP error codes.
- **Cron DST-gap occurrences are skipped** (library behavior). ACCEPT. Skipping an
  occurrence that lands in a spring-forward gap is defensible and rare; double-firing or
  shifting would be worse for a scheduler whose fires are idempotency-keyed on the scheduled
  instant.
- **Default compose tier writes the DEK-unwrapping host key as plaintext at rest.** ACCEPT
  (already loudly warned via `HostKeyPlaintext` in doctor). This is the known invariant-4
  tradeoff for the zero-config default tier. The separately-queued secret-staging (memfd)
  rework addresses runtime staging, not this at-rest default-tier choice, which stays a
  documented, warned tradeoff.

## Deferred follow-ups (noted, not blocking this review)
- Reactive/scheduled `shell` actions are now env-scoped (PATH-only) but still not routed
  through the sandbox driver — routing them through it is a larger change flagged during
  review.
- `approval link` picks up the daemon's persisted `--public-url`; the value is written as a
  `setting` row at daemon startup. No further work needed, noted for provenance.
