# ADR 0020 — Reload first, restart after drain, recover interrupted jobs

Status: accepted · 2026-10-09

## Context

A running daemon reads some state on every use and some state once into
memory. Schedules are the read-once case: `SIGHUP` and `--reload-interval`
rebuild the registrations from the database, but there is no `reload` verb and
the unit carries no `ExecReload`, so an operator who changed a schedule reaches
for a restart — which stops the scheduler, spends the drain grace, and retires
whatever is still owned to `interrupted`. A restart is also the only path when
only the binary or a `serve` flag changed, and an interrupted job today is
listed (`drain interrupted`) but never re-driven by anything.

## Decision

Prefer reload over restart, and restart only after drain, in three phases:

1. **Reload first (this change).** A first-class `reload`: `POST
   /api/v1/maintenance/reload` (operator scope, bearer-only, like `cordon`),
   served by the drain seam calling the existing `Scheduler.Reload`, and an
   `exocomp reload` CLI over `--remote` reporting the schedule count. It adds
   no second reload mechanism: a reload that cannot read the database leaves
   the running set alone, exactly as `SIGHUP` does today.
2. **Restart after drain (follow-up).** A restart path that drains first
   (cordon, wait until safe) and then re-execs, with safety checks refusing
   the restart while jobs are still in flight. The (Ansible-managed) unit
   gains `ExecReload=~/.local/bin/exocomp --remote self reload`; the
   unit file itself is not edited in this repo.
3. **Job recovery (follow-up).** The interrupted listing becomes something an
   operator re-drives from, rather than a read-only post-reboot view.

What needs which action:

| Change | Action |
|---|---|
| Secrets/redaction, mutes, approvals — read from the DB per use | live, no action |
| Schedules — read once into memory | `exocomp reload` (or `SIGHUP`) |
| Binary or `serve` flag change | restart, after drain |

The audit of which rows are read-once versus read-per-use stays a follow-up:
schedules are the only reloadable set named here.

## Consequences

- Schedule edits reach a running daemon without cancelling or retiring work.
- A restart stays available for what only a restart can change, and when it
  happens it happens on a drained daemon.
- `ExecReload` gives the unit a reload that is not a restart; the unit file
  stays Ansible-managed and out of this repo.
- Until phases 2 and 3 land, restarts keep today's semantics and interrupted
  jobs keep today's listing.
