export const meta = {
  name: 'exocomp-m3c-pass1',
  description: 'M3c pass 1: ADR-0008 step ledger + CEL plan-policy gate',
  phases: [{ title: 'Build', detail: 'ledger (opus) + plan-policy (opus)' }],
}
const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'
const WT = REPO + '/.worktrees'
const COMMON = `
Read <worktree>/AGENTS.md and <worktree>/.agents/workflows/m1-plan.md (the final "M3c" section = your ownership map + gate; read pass 1 AND the intro) FIRST. Also BINDING: docs/adr/0008-step-ledger-over-dbos.md (the ledger design you implement), docs/design/v1-acceptance.md (the five stories the milestone gates — esp. the governing rule "never expose raw state"), docs/design/expert-reviews.md. Conventions: lane-owned paths ONLY (never touch another lane's files or migration number); logical-unit commits via "git -C <worktree>"; conventional-commit titles (feat|fix|chore|docs|refactor|test|perf|build)(scope): summary, <= 52 chars; NO trailers (no Co-Authored-By / Signed-off-by / Generated-with); explicit staging (never git add -A); no-slop comments (explain WHY, match surrounding density); never sudo. The pre-commit hook runs the FULL make check (go vet + go test -race ./..., ~2-2.5 min) against the index on every commit — never bypass it, be patient, and expect commit-msg to reject a >52-char or non-conventional title. REUSE existing machinery, do not reinvent: the queue is the ONLY execution authority (a workflow drives it, it does not become a second runtime); store migration + CRUD patterns (see internal/store/migrations + the *_test.go); the reactive CEL pattern (internal/reactive/rule.go: celEnv via sync.OnceValues, Compile enforcing a bool AST output, Program.Match under cost+time budget); the approval gate (internal/queue/gate.go EnqueueGated -> awaiting_approval, Release/Withdraw); the secrets Envelope/Derive/Redactor; the sandbox tier (jobs run sandboxed). Subprocess success is exit-code-only; secrets never in argv/env-dumps/events/persisted-plaintext. Verify with REAL command output pasted into your report; add a mutation-check to any test that could pass vacuously.
Host: go 1.26, golangci-lint, tofu, ansible-playbook + ansible-galaxy, restic, age, sops, podman, limactl, nono 0.74.0. Never sudo.
`
const SCHEMA = {
  type: 'object', required: ['commits', 'summary', 'verified', 'concerns'],
  properties: {
    commits: { type: 'array', items: { type: 'object', required: ['sha', 'title'], properties: { sha: { type: 'string' }, title: { type: 'string' } } } },
    summary: { type: 'string' }, verified: { type: 'array', items: { type: 'string' } }, concerns: { type: 'array', items: { type: 'string' } },
  },
}
phase('Build')
const results = await parallel([
  () => agent(`${COMMON}
YOUR LANE: ledger. Worktree: ${WT}/m3c-ledger (branch m3c-ledger; do all git via "git -C ${WT}/m3c-ledger"). You build the SPINE of M3c — the ADR 0008 step ledger — that pass 2's choreographies ride on. Own EXCLUSIVELY: internal/store migration 0011 + a new internal/store/workflow.go; a new internal/queue/workflow.go; and the boot-sweep extension in internal/queue (extend the existing crash sweep, do not fork it).

Build, grounded in ADR 0008:
1. Migration 0011 (internal/store/migrations/0011_workflow.sql): a workflow_step table — workflow_id (TEXT), step_no (INT), kind (TEXT), status (TEXT: pending|running|succeeded|failed|blocked), result_json (TEXT), rollback_marker (TEXT NULL), created_at, updated_at; PRIMARY KEY (workflow_id, step_no); an index for sweeping incomplete workflows. Follow the exact migration + embed pattern of the existing files.
2. internal/store/workflow.go: CRUD — CreateWorkflow(steps), the step-status transitions written in the SAME sql tx as the caller's effect (expose a tx-taking method so the queue writes the ledger row and the job effect atomically — a crash between effect and ledger must not double-run an idempotent step), ListIncompleteWorkflows, GetWorkflow. Mirror internal/store/job.go conventions and wrap().
3. internal/queue/workflow.go: a sequencer over the EXISTING queue. Enqueue an ordered list of steps; run step N+1 only once step N reached succeeded; a failed step leaves later steps blocked and the workflow failed; persist each step's outcome to the ledger. The apply->play dependency EDGE is the minimum viable shape (step 2 depends on step 1 succeeding). Resume: in the boot sweep, reload incomplete workflows from the ledger and continue them WITHOUT re-running already-succeeded steps.
4. Idempotency: a step carries an idempotency key so a resume cannot double-apply; reuse job.go's idempotency_key plumbing.

GATE (paste real output): a two-step workflow where step 2 runs iff step 1 succeeded; step 1 failing leaves step 2 blocked and the workflow failed, both readable from the ledger; then a daemon-restart test — start a workflow, kill the daemon (or the sequencer) mid-flight AFTER step 1 succeeds, restart, and show step 1 is NOT re-run while step 2 completes from the ledger. Show the same-tx write (a test asserting the ledger row and the effect commit or roll back together). Full make check green. Report with the SCHEMA. Do NOT touch internal/executor, cmd, or any migration other than 0011.`,
    { label: 'lane:ledger', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: plan-policy. Worktree: ${WT}/m3c-policy (branch m3c-policy; all git via "git -C ${WT}/m3c-policy"). You add a CEL policy gate over OpenTofu plan output. Own EXCLUSIVELY: a new internal/policy/** package; internal/store migration 0012 + its CRUD (a new file, e.g. internal/store/policy.go); cmd/exocomp/policy.go; and the enforcement seam at plan-seal time (cmd/exocomp/planseal.go + the plan-seal call site only — a MINIMAL hook; do NOT rewrite internal/executor/tofu.go's plan path, only READ the plan-show JSON it already produces).

Build:
1. internal/policy: a CEL env over the tofu plan-show JSON, built on the reactive/rule.go pattern (celEnv via sync.OnceValues; Compile enforcing a bool AST output type; a Program with OptOptimize + CostLimit + InterruptCheckFrequency; Match(ctx, plan) under a time budget). The single variable is "plan" (map<string,dyn>) bound to the parsed plan-show JSON (resource_changes[], etc.). A compile error is returned at policy-ADD time so a rule that cannot decide can never silently allow.
2. Migration 0012 (0012_plan_policy.sql): plan_policy(id, name UNIQUE, workspace, expr, action CHECK IN ('deny','require_approval'), enabled, created_at). Store CRUD in internal/store/policy.go.
3. cmd/exocomp/policy.go: policy add <name> --workspace <w> --expr <cel> --action deny|require_approval, policy list, policy rm. Validate the expr compiles before persisting.
4. Enforcement seam at plan-seal: when a plan artifact is sealed, evaluate the workspace's enabled policies against the plan-show JSON. A "deny" verdict marks the sealed plan UNCONSUMABLE (a later apply of that plan refuses with the policy name in the message). A "require_approval" verdict routes the apply through the EXISTING queue approval gate (internal/queue/gate.go) — do not build a second gate. Record the verdict + matched policy on the sealed-plan record/event so it is auditable and redaction-safe (no plan secrets in the event).

GATE (paste real transcripts): a deny policy whose expr matches a delete-bearing plan (e.g. plan.resource_changes.exists(c, c.change.actions.exists(a, a == "delete"))) makes the sealed plan unconsumable and a subsequent apply of it REFUSES naming the policy; a require_approval policy routes the apply through the approval gate; a non-matching plan applies clean; policy add rejects an invalid CEL expr. Full make check green. Report with the SCHEMA. Do NOT touch internal/queue/workflow.go, internal/store migration 0011, internal/executor/tofu.go's plan production, or any other lane's files.`,
    { label: 'lane:plan-policy', phase: 'Build', model: 'opus', schema: SCHEMA }),
])
return { lanes: results.filter(Boolean) }
