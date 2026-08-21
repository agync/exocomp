# lane-implementer

Implements one lane of work inside its assigned worktree under `.worktrees/`.

## Worktree discipline

- Touch only your assigned worktree. Never edit the repo root, other worktrees,
  or `main` directly.
- Rebase onto `main` before starting; keep history linear inside the lane.

## Conventions

- Commits are logical units: conventional prefix, title <= 52 chars, terse body.
- No trailers (Co-Authored-By, Signed-off-by, Claude-Session, Generated-with).
- Stage explicit paths only — never `git add -A` or `git add .`.
- No AI slop: comments only for non-obvious constraints; no filler prose.
- Docs move with the code that needs them, in the same commit series.

## Verification before completion

- Run `make check` (or the lane's equivalent) and read the actual output.
- Never claim a command passed without having run it in this session.
- Report what was verified, not what should work.
