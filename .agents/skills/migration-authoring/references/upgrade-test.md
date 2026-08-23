# The pinned in-place-upgrade test

A migration that introduces a table or column gets a
`TestMigrationNNNNUpgradesFromMM` in `internal/store`, where `MM` is the version
immediately before yours. Live models: `TestMigration0018UpgradesFrom17`
(`runner_test.go`) and `TestMigration0019UpgradesFrom18` (`clitool_test.go`).
Copy one.

## What it must prove

A migration that runs only on a fresh file is untested for the real case: an
operator opening an existing database with a newer binary. The test forces that
path.

1. Pin a scratch database at `MM` with the shared helper
   `pinAtMigrations(t, dbPath, MM)` (defined in `secret_lifecycle_test.go`). It
   replays every embedded migration up to and including `MM` against a raw
   `sql.Open`, then stamps `PRAGMA user_version = MM`.
2. On a raw `sql.Open` (before running your migration), assert the thing your
   migration introduces is **absent** at `MM` — e.g. probe `sqlite_master` for
   the new table name and require count 0. This proves the pin is honest and the
   change is genuinely yours.
3. `Open(dbPath)` with the current binary — this triggers `migrate`.
4. Assert `SchemaVersion(ctx) >= NNNN`.
5. Exercise the new schema through the store's own CRUD (create then list/read),
   not just `sqlite_master`. Usable, not merely present — the models use
   `CreateRunner`/`ListRunners` and `CreateCLITool`/`ListCLITools`.

## Constraints

- `t.Parallel()` and `t.TempDir()`, as the existing upgrade tests do — no shared
  on-disk fixture.
- The test name's `NNNN` must track the file: if integration renumbers your
  migration on a collision, rename the test in the same move (see SKILL.md).
- Keep it to the one upgrade path. Broad behaviour of the feature belongs in the
  feature's own test file; this test's single job is the in-place jump from `MM`.
