---
name: migration-authoring
description: Add a schema migration to the SQLite store safely. Use when a change touches internal/store/migrations/ — a new table, column, index, or an enum CHECK that must widen. Covers the forward-only NNNN_name.sql + PRAGMA user_version mechanism, contended numbering that only settles at merge, and the pinned in-place-upgrade test each introduction migration must carry.
---

# Migration authoring

The store is embedded SQLite. Schema evolves by adding one file to
`internal/store/migrations/`; there is no ORM, no down-migration, and no
external tool. `internal/store/migrate.go` embeds the directory with a
`//go:embed migrations/*.sql` glob, parses each name as `NNNN_name.sql`, sorts
by the integer `NNNN`, and applies every file whose number is above the
database's `PRAGMA user_version` — each in its own transaction that re-reads
`user_version` under the write lock and then stamps it to `NNNN`. So a file is a
migration purely by living in that directory with a numeric prefix; nothing
registers it.

## The rules, and why

- **Forward-only, one logical change per file.** A database whose `user_version`
  is *ahead* of the binary is a hard error in `migrate`, not a downgrade — there
  is no rollback path, so a file ships or it does not. Keep each file to a single
  coherent schema change so a bisect lands on one cause and the paired upgrade
  test asserts one thing.

- **Pick the next free `NNNN`, but treat the number as not yours until merge.**
  Numbering is contended: two parallel lanes both reading "highest is 0018" will
  both write `0019`. That happened — `0019_cli_tool` and a `0019_schedule_drill`
  collided; integration renumbered the later one to `0020_schedule_drill`. The
  number is only final at MERGE. On rebase/integrate, if your `NNNN` now clashes,
  the integrator renames the later file and updates its paired test in the same
  move — the test name's `NNNN` and any comment that cites the number — so file
  and test stay in lockstep. Load-bearing consequence: do not hardcode your
  migration number anywhere it cannot follow a rename — keep it in the file name,
  the test name, and cited comments, nowhere else. This is the same collision the
  integration gate (the **lane-integration** skill) resolves at merge, where
  `loadMigrations` refuses a duplicate version outright (`migrations A and B
  share version N`); authoring defensively is what keeps that resolution cheap.

- **Enum-like columns get a CHECK.** A column with a fixed value set is
  constrained inline — e.g. `egress TEXT NOT NULL DEFAULT 'none' CHECK (egress IN
  ('none', 'https'))` in `0019_cli_tool.sql`. The database, not the Go layer, is
  the last line that rejects an out-of-set value.

- **An introduction migration carries a pinned upgrade test.** A migration that
  adds a table or column and only runs on a fresh database is not proven for the
  operators who matter — the ones upgrading in place. Add
  `TestMigrationNNNNUpgradesFromMM` (MM = the version before yours) that pins an
  old `user_version`, opens with the current binary, and asserts a clean in-place
  upgrade plus that the new schema is actually usable. A rebuild migration
  (widening a CHECK) carries a test too, but its risk is different — see below.
  → `references/upgrade-test.md`

## Widening a CHECK, or any column-level change

SQLite cannot alter a CHECK in place. Widening an enum means rebuilding the
table, which has its own safety precondition (nothing may reference the table
being dropped) and its own failure mode (row loss on copy). Do not improvise
this. → `references/rebuild-table.md`
