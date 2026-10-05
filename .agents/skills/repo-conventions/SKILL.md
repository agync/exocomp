---
name: repo-conventions
description: Machine-discoverable copy of AGENTS.md invariants 9-12 — commit hygiene, no-slop rules, worktree workflow, and hooksPath activation for this repo. Use before committing, opening a worktree, or writing docs/comments here.
---

# Repo conventions

This is the machine-discoverable copy of `AGENTS.md` invariants 9-12. If this
file and `AGENTS.md` disagree, `AGENTS.md` wins.

## Commit rules

- Logical units: one coherent change per commit, not a batch of unrelated edits.
- Conventional style title, `<= 52` chars:
  `^(feat|fix|chore|docs|refactor|test|perf|build)(\([a-z0-9./-]+\))?: .+`
- Body (optional): terse, states what the change does, wrapped for readability.
- No trailers: no `Co-Authored-By`, `Signed-off-by`, `Claude-Session`,
  `Generated-with`/`Generated with`, no meta-messages.
- Stage explicit paths only. Never `git add -A` or `git add .`.

## No-slop rules

- Code comments only for constraints the code itself cannot show (why, not what).
- No narrating comments (`// increment i`), no filler or marketing prose in
  docs, commit messages, or code.

## Worktree workflow

- Parallel work happens in worktrees under `.worktrees/`.
- `main` stays linear: rebase then fast-forward. Merge commits are reserved
  for milestone integrations.
- Work only inside your assigned worktree; never touch the repo root, other
  worktrees, or push directly to `main`.

## Hooks activation

Commits are gated by the fast `make check` (fmt, vet, lint, tidy, `go test
-short`) and the commit-message rules above via the pre-commit hooks; the
full `make check-race` (`-race ./...`) runs at pre-push and worktree merges.
Activate them once per clone:

```sh
pip install pre-commit
make hooks
```
