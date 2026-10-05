# Contributing

How work lands in exocomp, and what gates it. The contract lives in
[AGENTS.md](../../AGENTS.md); this page is the working procedure it references.

## The contract

- **AGENTS.md is the entrypoint.** The twelve invariants fail review, whatever
  the quality of the change otherwise. Read it before touching anything.
- **Contributing: no trailers.** Commits are logical units with conventional
  titles; there is no `Signed-off-by` trailer — invariant 9 admits no trailers at
  all.
- **Docs move with the change** (invariant 8). `docs/` is an OKF v0.2 bundle;
  update it in the same commit series as the code it describes. If docs and
  code disagree, code wins and the correction lands in [log.md](../log.md).

## Commit conventions

- Logical units, one coherent change per commit.
- Conventional style, title at most 52 chars; terse body stating what the
  change does. No trailers, no meta-messages (invariant 9).
- Stage explicit paths only. Never `git add -A`.
- Every commit runs the fast gate: the pre-commit hook checks the index out
  into a temp tree and runs `make check` (fmt, vet, lint, tidy, `go test
  -short`), and the commit-msg hook rejects a non-conventional or over-long
  title. The full race suite runs at pre-push and before merging a worktree
  (`make check-race`). Activate the hooks once per clone or worktree with
  `git config core.hooksPath .agents/hooks`.

## Worktree workflow

- Parallel work happens in worktrees under `.worktrees/`; `main` stays linear
  — rebase then fast-forward, merge commits only for milestone integrations
  (invariant 11).
- A lane owns its files and claims its migration number provisionally; the
  integrator renumbers a collision at merge. Work only inside your assigned
  worktree; never the repo root or another lane's.

## Verification

- A change is done only when `make check` passes, with the real output pasted;
  the full `make check-race` is the pre-push / merge-integration gate.
- Guard tests carry a mutation check, so a test that passes vacuously (without
  the code under test) is caught.

## Design and review

- Architecture-relevant changes record decisions as ADRs, one decision per
  file under [adr/](../adr/index.md).
- Specs for in-flight work live under `.agents/brain/specs/` — a local-only,
  untracked working directory, so a clone of this repo carries no spec store;
  design proposals live under [design/](../design/index.md).
- Scope is stated in one sentence before work begins — unit of work, target,
  outcome. Anything not needed for that outcome is out of scope by default.
- A clean subagent reviews a change as a skeptical maintainer before it
  integrates; the operator holds the final decision.
