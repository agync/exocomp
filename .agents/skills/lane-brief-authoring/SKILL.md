---
name: lane-brief-authoring
description: Write the brief that dispatches one worktree lane so its work lands clean and passes the merge gate. Use when assigning a subagent a lane under .worktrees/ or assembling an mNN-build workflow — it fixes the file-ownership, test-first, commit, report, model, concurrency, and migration clauses a lane needs. Pairs with the lane-implementer agent that consumes the brief.
---

# Lane-brief authoring

A lane brief is the whole contract a subagent gets. It runs alone in one
worktree while sibling lanes edit their own files concurrently, so anything the
brief leaves implicit becomes a merge conflict or a gate failure that surfaces
only after the agent reports done. Write the brief so a lane that follows it
literally cannot collide with a sibling, cannot claim an unrun check, and cannot
land a commit the hooks reject.

The gate is not advisory: the pre-commit hook checks the index out into a temp
tree (`git checkout-index`) and runs the full `make check` (fmt-check, vet,
lint, tidy-check, test) there on every commit — never against the working tree,
so unstaged edits neither mask nor cause a failure. commit-msg rejects a title
over 52 chars or off the conventional prefix. A brief that ignores either
produces an agent that burns its budget fighting the hook.

## Run this checklist down

Every clause below must be answered in the brief before dispatch. A missing
clause is a defect, not a default.

1. **Ownership, both directions.** State the files/dirs this lane OWNS
   exclusively and, separately, the sibling paths it must AVOID by name.
   Siblings are live — "touch only your files" is not enough when the agent
   cannot see the other lanes; name the packages, migration numbers, and shared
   files that belong to someone else this round.
2. **Worktree address.** The orchestrator creates the lane's worktree BEFORE
   dispatch — a lane that self-checks for its worktree, finds it missing, and
   stops has burned a full round-trip for nothing. Give the worktree path
   (`.worktrees/mNN-<lane>`), the branch (`mNN/<lane>`), and the instruction to
   run all git as `git -C <worktree>`. Tell the lane to write only to absolute
   paths inside that worktree — never the repo root or another lane's worktree
   (AGENTS.md invariant 11); a verify-agent once leaked files into the repo
   root this way and blocked the merge until a human ran `git clean`.
3. **Test-first with REAL output.** Demand the test written before the fix and
   the actual pasted command output in the report — not a claim that it passes.
   Name the failure the test must first exhibit, so a vacuously-green test is
   caught. Add: a mutation check on any guard test that could pass without the
   code under test.
4. **Commit hygiene.** Logical-unit commits; conventional title
   `<= 52` chars; NO trailers (no Co-Authored-By, Signed-off-by, Claude-Session,
   Generated-with); explicit staging, never `git add -A` (AGENTS.md invariant 9;
   the `repo-conventions` skill is the machine-readable copy).
5. **Structured report schema.** Give the lane a fixed output schema so the
   dispatcher can machine-read every lane the same way — commits, summary,
   verified, concerns. Use the schema in `references/brief-template.md`.
6. **Model and effort to the task.** opus at high effort for hard correctness
   and security work (gates, crypto, idempotency, concurrency, migrations);
   sonnet for mechanical or scoped edits (wiring, renames, docs, a single CRUD
   method). State it in the brief; do not leave it to the runner default.
7. **Concurrency-aware gating.** The gate runs the whole race-enabled suite
   (`go test -race ./...`) and takes a couple of minutes; several lanes commit
   at once. Tell the lane to set generous gate timeouts and be patient, never to
   bypass the hook. Allow ONE retry of a KNOWN flake — the `flake-triage` skill
   is this repo's source of truth for which failures qualify (e.g.
   `TestTofuPlanRunsOfflineFromPluginCache`, the plugin-cache/`unshare` exec race
   under concurrent gating); point the lane at it to tell a flake from a real
   failure. A second failure of the same check is a real failure.
8. **Migration lanes: claim now, final at merge.** If the lane adds a store
   migration, tell it to claim the NEXT FREE
   `internal/store/migrations/NNNN_name.sql` number at start — but the number
   is only provisional until the lane merges. Parallel lanes routinely claim
   the same number, since neither can see the other's worktree; that is
   expected, not a mistake by either lane, and `loadMigrations` would reject
   the resulting duplicate (`store: migrations A and B share version N`) if it
   ever reached `main` unresolved. The `lane-integration` skill's integrator
   catches this at merge and renumbers whichever lane lands second, so the
   brief does not need the lane to preempt a collision it cannot see. Point at
   the `migration-authoring` skill for the file shape and the forward-only rule.

## After the lane reports

The brief ends the lane; it does not merge it. How the result rebases onto the
linear `main` and clears the integration gate is the `lane-integration` skill's
job — hand the report and worktree to it. Reference that skill in the brief so
the lane knows its commits will be replayed, not merged as-is, and keeps history
clean enough to rebase.

Dispatch the integrator only after the lane's WORKFLOW-COMPLETION
notification — never off a mid-flight "clean" heartbeat snapshot. A lane can
be transiently clean between two of its own commits while more work is still
coming; merging off that snapshot integrates a lane that is not actually done.

The fill-in skeleton, the report schema, and a worked ownership block are in
`references/brief-template.md` — open it when you sit down to write a brief.
