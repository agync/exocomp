# Lane-brief template

Fill every bracketed slot. An empty slot means the clause was skipped — that is
the defect this template exists to prevent. Order is deliberate: shared rules
first (identical across sibling lanes so they can be one `COMMON` string), then
the per-lane block, then the gate, then the schema.

## COMMON block (shared verbatim across all lanes of one dispatch)

```
Read <worktree>/AGENTS.md FIRST, plus [any BINDING record / design doc this
milestone gates]. Conventions: lane-owned paths ONLY (never another lane's
files or migration number); logical-unit commits via "git -C <worktree>";
conventional titles (feat|fix|chore|docs|refactor|test|perf|build)(scope):
summary, <= 52 chars; NO trailers (no Co-Authored-By / Signed-off-by /
Generated-with); explicit staging (never git add -A); no-slop comments (WHY
not what, match surrounding density); never sudo. The pre-commit hook runs the
FAST make check against the index on EVERY commit (`go test -short`); the full
`make check-race` is the integration gate at pre-push and merge — never bypass
it, be patient, and expect commit-msg to reject a >52-char or
non-conventional title. REUSE existing machinery; do not reinvent. The queue is
the only execution authority. Subprocess success is exit-code-only; secrets
never in argv / env dumps / events / persisted plaintext. Verify with REAL
pasted command output; add a mutation check to any test that could pass
vacuously.
Host: [pinned toolchain + versions the lane may assume]. Never sudo.
```

## Per-lane block

```
YOUR LANE: <name>. Worktree: <REPO>/.worktrees/mNN-<lane>
(branch mNN/<lane>; do ALL git via "git -C <REPO>/.worktrees/mNN-<lane>").
One line on why this lane exists / what rides on it.

OWN EXCLUSIVELY: <exact files and dirs — packages, the specific migration
number, the specific CLI file, the one call site>.

Build:
1. <numbered, testable steps grounded in the binding spec>
2. ...

Model/effort: <opus high | sonnet> — because <hard correctness+security vs
mechanical>.

GATE (paste REAL output): <the exact behaviours a reviewer re-runs — the
failing case the test first shows, the transitions, the restart/idempotency
proof>. `make check` green (fast); the full `make check-race` runs at
integration. Report with the SCHEMA.

Do NOT touch: <sibling paths by name — the packages, migrations, and shared
files owned by other lanes this round>. If you add a migration, claim the NEXT
FREE internal/store/migrations/NNNN_name.sql number now; it is provisional
until merge — a sibling lane may claim the same number in parallel, and the
integrator (lane-integration skill) renumbers whichever lane lands second, so
you do not need to re-check it yourself. Retry a KNOWN flake once (see the flake-triage skill,
this repo's flake source of truth — e.g. TestTofuPlanRunsOfflineFromPluginCache
under concurrent gating); a second failure of the same check is real.
```

## Report SCHEMA (give every lane the same one)

```json
{
  "type": "object",
  "required": ["commits", "summary", "verified", "concerns"],
  "properties": {
    "commits": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["sha", "title"],
        "properties": { "sha": { "type": "string" }, "title": { "type": "string" } }
      }
    },
    "summary": { "type": "string" },
    "verified": { "type": "array", "items": { "type": "string" } },
    "concerns": { "type": "array", "items": { "type": "string" } }
  }
}
```

`verified` holds the checks the lane actually ran with output pasted, not the
checks it believes would pass. `concerns` is where a lane flags a sibling seam
it could not fully test — the integrator reads it first.

## Worked ownership block (why AVOID is stated separately)

A lane told only "own internal/policy" still does not know that migration 0012
is its number and 0011 is the ledger lane's, or that it may READ
`internal/executor/tofu.go` but must not rewrite it. Spell both:

```
OWN: a new internal/policy/** package; migration 0012 + internal/store/policy.go;
     cmd/exocomp/policy.go; the plan-seal enforcement hook (the call site only).
AVOID: internal/queue/workflow.go and migration 0011 (ledger lane);
       internal/executor/tofu.go's plan production (READ its JSON, do not touch).
```

The OWN list prevents under-reach; the AVOID list prevents the collision the OWN
list alone cannot, because the agent cannot see the sibling lanes.
