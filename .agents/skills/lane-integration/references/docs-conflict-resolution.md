# The only conflicts the integrator resolves

Four docs files conflict on nearly every parallel-lane rebase because both
lanes append to them. Their merge is a union with a known shape, so the
integrator resolves them itself. Every other file — code above all — is out of
scope: a conflict there aborts the rebase (see `protocol.md` step 2).

The test for "may I union this myself" is not "is it docs" — it is "is the
correct merge mechanical and content-free". These four are. A prose section
that two lanes rewrote differently is not; hand that back.

## `docs/log.md` — keep every entry, newest first

`docs/log.md` is an append log, **newest first** (its own header says so, and
the docs-curator agent enforces it: one entry per event, dated). Two lanes each
prepend a dated entry; the conflict is the two new blocks fighting for the top.

Resolution: **keep both entries**, ordered newest-first by their dates, above
the previously-latest entry. When both entries share the same date, that date
alone does not order them: the later-integrating branch's entry goes on top,
above the entries already on main. Never drop or merge two distinct entries
into one — each records a separate event. Preserve each block's exact text and
its trailing relative link line.

## The roadmap under `docs/design/` — union the rows

Currently `docs/design/v1.1-roadmap.md`. Two lanes each add or tick a row.

Resolution: **union the rows** — keep every row both sides added, and if both
edited the *status* of the same existing row, that is no longer mechanical:
STOP and hand back rather than choosing a status. Adding distinct rows is
mechanical; disagreeing on one row's state is a semantic choice you do not make.

## `docs/operations/CHANGELOG.md` — keep every entry, newest first

The software release narrative, newest first like `docs/log.md`. Two lanes each
prepend a dated entry; the conflict is the two new blocks fighting for the top.

Resolution: **keep both entries**, ordered newest-first by their dates, above
the previously-latest entry. When both entries share the same date, the
later-integrating branch's entry goes on top. Never drop or merge two distinct
entries into one — each records a separate release change.

## `docs/operations/index.md` — union the table rows

`docs/operations/index.md` is the operations map: a `| Page | Covers |` table,
one row per operations page. A lane that adds an operations page adds its row
here in the same commit (invariant 8, docs move with the change).

Resolution: **union the rows** — keep every page row from both sides. Order
follows the existing grouping in the file; if unsure, append the new rows in the
order the lanes landed. Do not drop a row, and do not rewrite the `Covers` cell
of a row you did not add.

## After any union

`git add` the resolved path and `git rebase --continue`. The union is a
resolution, not a new authored change — do not add commentary, reflow unrelated
lines, or "tidy" the surrounding file. If resolving one of these four ever
requires a judgment about wording or status rather than concatenating entries or
rows, it has stopped being mechanical: abort and report.
