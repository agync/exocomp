---
type: Guide
title: "Worktree lanes and branch hygiene"
description: "Parallel development with Git worktrees under .worktrees/, linear history on main, and lane brief conventions."
status: stable
tags: [contributing, git, worktrees, branches, lanes]
---

# Worktree lanes and branch hygiene

exocomp enforces Invariant 11: **Parallel work happens in worktrees under `.worktrees/`; `main` stays linear (rebase then fast-forward; merge commits only for milestone integrations)**.

Working directly in the root repository or dirtying `main` is prohibited.

## Opening a worktree lane

To start work on a feature, bug fix, or subagent task, create an isolated worktree under `.worktrees/`:

```bash
git worktree add -b feat/my-lane .worktrees/my-lane main
```

Navigate into `.worktrees/my-lane/` to make changes. Each lane:
- Has an explicit scope defined in a lane brief.
- Owns a disjoint set of files to prevent merge conflicts.
- Claims a provisional migration number if touching the database schema.

## Commit conventions (Invariant 9)

Every commit must be a logical unit:
- **Title**: Conventional commit style, at most 52 characters.
  Examples:
  `feat(queue): add fifo priority weighting`
  `fix(store): resolve migration sequence collision`
  `docs(guide): add mobile pairing walkthrough`
- **Body**: Terse and descriptive of what the change does.
- **No trailers**: No `Signed-off-by`, `Co-authored-by`, or PR metadata.
- **Explicit staging only**: Never run `git add -A` or `git add .`. Stage exact file paths.

## Integrating a worktree into `main`

When work in a lane is verified and passes `make check-race`:

1. Rebase the lane against current `main`:
   ```bash
   git fetch origin main
   git rebase origin/main
   ```
2. Fast-forward merge `main`:
   ```bash
   git checkout main
   git merge --ff-only feat/my-lane
   ```
3. Tear down the worktree:
   ```bash
   git worktree remove .worktrees/my-lane
   git branch -d feat/my-lane
   ```
