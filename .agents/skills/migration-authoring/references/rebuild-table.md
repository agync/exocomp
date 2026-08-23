# Widening a CHECK by rebuilding the table

SQLite has no `ALTER TABLE … ALTER CONSTRAINT`. To widen (or narrow) a CHECK —
typically adding a value to an enum column — the table is rebuilt. The worked
example is `0020_schedule_drill.sql`, which added `'drill'` to the schedule
`kind` CHECK; `0015_schedule_play.sql` is the earlier rebuild it mirrors (which
in turn mirrors `0005`).

## The pattern

Inside the single migration file (it runs in one transaction):

1. `CREATE TABLE <name>_vN (…)` — the full definition with the widened CHECK, a
   new suffix each time so successive rebuilds never collide (`0015` made
   `schedule_v3`, `0020` made `schedule_v4`).
2. `INSERT INTO <name>_vN (cols…) SELECT cols… FROM <name>` — copy every row,
   naming columns explicitly.
3. `DROP TABLE <name>` then `ALTER TABLE <name>_vN RENAME TO <name>`.
4. Recreate every index the old table had (the drop takes them with it) — e.g.
   `CREATE INDEX schedule_enabled ON schedule (enabled)`.

## The precondition that makes it safe

`dsn` opens the database with `_pragma=foreign_keys(ON)`, and a migration runs
inside a transaction where toggling that pragma is a no-op. So dropping a table
is only safe when **nothing else references it by foreign key** — the outgoing
references the table declares are fine, but an incoming FK from another table
would be orphaned or block the drop. `0020_schedule_drill.sql` states this
directly: "nothing references schedule, which is what makes that safe." Verify
that no other table has a `REFERENCES <name>` before choosing rebuild; if
something does, the change is larger than one migration and needs its own design.

## The test a rebuild owes

The rebuild-specific failure mode is data loss: step 2 copies rows, and a
mistaken column list or an INSERT that silently drops rows is exactly what the
pattern risks. So a rebuild's test must assert that rows written **before** the
rebuild survive it — the part a plain "the new value is now insertable" check
does not cover.

The live drill coverage today is `TestScheduleDrillKindValidates` (package
`store_test`, `schedule_drill_kind_test.go`): it creates a `kind = 'drill'`
schedule and confirms the widened CHECK admits it. That proves the CHECK, not
row survival across the rebuild — so when you author a rebuild, add the pinned
in-place jump from `MM` (→ `upgrade-test.md`) and have it seed a row at `MM`,
`Open` to trigger the rebuild, and read that row back intact.
