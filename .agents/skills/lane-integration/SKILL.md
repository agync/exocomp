---
name: lane-integration
description: Land a finished lane worktree onto main the way this repo requires — clean-check, rebase resolving only docs unions, migration-number collision check, fast-forward, one make check-race, then tear down. Use when integrating a reviewed .worktrees/ branch, or driving the lane-integrator agent. Stops and reports on any dirty worktree, Go conflict, or real test failure instead of guessing.
---

# Lane integration

Moving a reviewed lane from `.worktrees/<lane>` onto `main` is mechanical right
up until it is not. This skill is the protocol that makes the mechanical part
repeatable and forces a hard stop at the first non-mechanical step, so an
integrator never silently destroys uncommitted work, guesses at a `.go`
conflict, or masks a real test failure.

It integrates; it does not author. The lane's correctness was decided at review;
nothing here re-litigates it, and nothing here writes a fix.

## Invariants that gate the whole run

- **One integrator on `main` at a time.** The protocol ends in a fast-forward of
  `main`; two runs racing it corrupt history and lose a lane. Confirm no other
  integration is in flight before step 4.
- **`main` stays linear** (AGENTS.md invariant 11): rebase the lane, then
  fast-forward. No merge commit for an ordinary lane.
- **The integrator never invents fixes.** A dirty worktree, a code conflict, or
  a genuine `make check-race` failure is reported and left for the lane owner. It is
  never resolved by editing the lane's code or tests.

## The six steps

Run them in order. Read `references/protocol.md` for the exact command and the
stop condition at each step — the detail is there precisely so this file stays
short. Do not improvise past a stop condition.

1. **Clean-first.** `git -C <wt> status --short` must be empty. Dirty → STOP and
   report; never commit or discard another lane's work.
2. **Rebase onto `main`.** Resolve **only** the whitelisted docs unions
   (`references/docs-conflict-resolution.md`). A conflict in any `.go` file →
   ABORT, report the hunks verbatim.
3. **Migration-collision check.** `ls internal/store/migrations/`; no two files
   may share the `NNNN` prefix. Two lanes that branched before either merged can
   both claim the same number — renumber the later one and move its test.
4. **Fast-forward.** `git merge --ff-only` from the repo root. Refused → STOP.
5. **Check once.** Run `make check-race` at the integrated tip — the full gate
    invariant 12 wires into the pre-push hook and worktree merges. Retry at most
    **once**, and only for a timing flake the flake-triage skill lists. A real
    failure → report in full; do not revert or fix.
6. **Tear down.** Green only: remove the worktree, delete the branch.

## Do not duplicate the flaky list

Step 5's single retry is allowed only for a test already known-flaky. That list
lives in the **flake-triage** skill and is authoritative there. Do not copy test
names into this skill — a stale copy would license retrying a real regression.
If a failing test is not on that list, it is a real failure.
