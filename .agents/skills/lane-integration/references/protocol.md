# Integration protocol — step detail

Every step names its command and its stop condition. A stop condition ends the
run: report what you found and hand back. Do not proceed past one, and do not
work around it by editing the lane.

Throughout, `<wt>` is the lane worktree path under `.worktrees/` and `<branch>`
is its branch. Run repo-root commands from the main checkout, not the worktree.

## 1. Prove the worktree is clean

```sh
git -C <wt> status --short
```

Empty output → proceed. Any output → **STOP**. The worktree holds uncommitted
work that is not yours to judge. Never `commit`, `stash`, `checkout --`, `clean`,
or `reset` it to make the tree tidy — that destroys another lane's state on a
guess. Report the exact `status --short` lines and stop.

## 2. Rebase the lane onto main

```sh
git -C <wt> fetch origin            # if the lane tracks a remote; else skip
git -C <wt> rebase main
```

A clean rebase → proceed to step 3.

On a conflict, the file decides what you may do:

- **A whitelisted docs file** (`docs/log.md`, the roadmap under `docs/design/`,
  `docs/operations/index.md`) → resolve it as a union per
  `docs-conflict-resolution.md`, `git add` that path, `git rebase --continue`.
- **Any `.go` file, or any other file whose meaning you cannot union
  mechanically** → **ABORT**: `git rebase --abort`, then report the conflicting
  hunks verbatim (the full `<<<<<<< / ======= / >>>>>>>` blocks) and stop. The
  lane owner resolves code semantics. You do not pick a side, and you do not
  "just take theirs/ours".

## 3. Migration-number collision check

The store loads migrations by the integer prefix of each `internal/store/
migrations/NNNN_name.sql` file (`loadMigrations` in `internal/store/migrate.go`,
via `//go:embed migrations/*.sql`). It cuts each name at the first `_`, parses
the prefix as an int, keeps a `seen` map of versions, and returns
`store: migrations <a> and <b> share version <n>` on a duplicate — so a
collision fails `make check` at store load, not at runtime.

Two lanes that each branched off `main` before the other merged can both add,
say, `0019_*.sql`. After the rebase this is now a real duplicate:

```sh
ls internal/store/migrations/
```

If two files share a `NNNN`:

- The lane you are integrating is the **later** arrival (the other already
  landed on `main`). Renumber its file to the next free slot — e.g. `0019_*` →
  `0021_*` if `0020` is taken.
- Move any migration-versioned test with it. The suite names an upgrade test by
  the version span it exercises (e.g. `TestMigration0001To0002UpgradesExistingDB`
  in `internal/store/migrate_test.go`); if a test pins the number you renumbered,
  a rename that leaves it asserting the old version is a real failure, not a
  flake.
- This is the one code-adjacent edit the integrator makes, because it is
  mechanical and forced by the collision — not a semantic choice. If renumbering
  is not mechanical (the migration body references its own number, ordering
  matters against the sibling), STOP and hand back instead.

No collision → proceed.

## 4. Fast-forward main

```sh
git checkout main
git merge --ff-only <branch>
```

Fast-forward succeeds → proceed. `--ff-only` **refused** → **STOP**: `main`
advanced under you (another integrator, or the rebase in step 2 did not actually
put the lane ahead of `main`). Do not fall back to a real merge commit — that
breaks invariant 11. Report and stop; the lane needs re-rebasing.

A refusal quoting `untracked working tree files would be overwritten` is a
different failure from a non-fast-forward `main` — **STOP** the same way, but
report the exact paths it names. Never delete or overwrite them yourself: file
deletion of repo content may be guarded, and clearing them is a human call, not
an integrator one.

## 5. Run make check once

```sh
make check
```

This is the same gate `.agents/hooks/pre-commit` runs (AGENTS.md invariant 12);
run it explicitly here because a fast-forward creates no commit and so fires no
hook. Full run is ~2.5 min solo.

Run it to completion in the foreground, in this same turn, with a generous
timeout (up to 600000 ms) — do not background it and pause to wait on it. An
integrator agent does not reliably resume after a background pause, and a run
stranded mid-check leaves teardown undone and the worktree lingering.

- **Green** → proceed to teardown in this same turn — do not end the turn
  between the gate passing and step 6's cleanup.
- **Failure** → consult the flake-triage skill's flaky list. If, and only if,
  every failing test is on that list, re-run `make check` **once**. Still failing,
  or the failing test is not listed → **STOP**: report the full failing output.
  Do not revert the lane, patch the test, skip it, or edit code to force green. A
  real failure is a finding for the lane owner.

## 6. Tear down

Green `make check` only:

```sh
git worktree remove <wt>
git branch -d <branch>
```

Use `-d` (not `-D`): it refuses to delete a branch not merged into `main`, a last
guard that you actually landed the work. If `-d` refuses, the fast-forward did
not take — investigate, do not force with `-D`.
