---
type: Guide
title: "Schema migrations"
description: "Authoring forward-only SQLite migrations, managing PRAGMA user_version, and collision handling."
status: stable
tags: [contributing, sqlite, migrations, store, schema]
---

# Schema migrations

exocomp stores all queue, secrets, and execution state in a single SQLite database (`exocomp.db`) managed through forward-only SQL migrations under `internal/store/migrations/`.

## The migration mechanism

- **Forward-only**: Migrations are strictly additive. Rollbacks in production restore an automated pre-upgrade snapshot rather than running down-migrations.
- **Naming**: `NNNN_description.sql` (e.g. `0015_add_check_probes.sql`).
- **State tracking**: SQLite's native `PRAGMA user_version` tracks the currently applied migration level.
- **Single transaction**: Every migration runs inside an isolated transaction. A migration failure halts startup and rolls back immediately.

## Authoring a migration

1. Create a new file under `internal/store/migrations/`:
   ```sql
   -- internal/store/migrations/0021_netgroup_cidr_indexes.sql
   CREATE INDEX IF NOT EXISTS idx_netgroups_cidr ON netgroups(cidr);
   ```

2. Numbering contention across worktrees:
   When developing in a parallel `.worktrees/` branch, choose the next sequential number provisionally. If another branch merges first, the integrator renumbers the file before fast-forwarding to `main`.

3. Migration upgrade tests:
   Every migration must be covered by the migration test suite (`internal/store/migration_test.go`), asserting:
   - Clean application on an empty database.
   - In-place upgrade of a database populated with sample rows from version `N-1`.
   - Preservation of existing data without corruption.
