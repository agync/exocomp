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

1. **Reload first (landed).** A first-class `reload`: `POST
   /api/v1/maintenance/reload` (operator scope, bearer-only, like `cordon`),
   served by the drain seam calling the existing `Scheduler.Reload`, and an
   `exocomp reload` CLI over `--remote` reporting the schedule count. It adds
   no second reload mechanism: a reload that cannot read the database leaves
   the running set alone, exactly as `SIGHUP` does today.
2. **Restart after drain (this change).** `exocomp restart` and
   `POST /api/v1/maintenance/restart` (operator scope, bearer-only) cordon,
   return at once, and drain in the daemon's background: when the in-flight
   count reaches zero the daemon runs the ordinary clean shutdown and re-execs
   the on-disk binary in place — same pid, no systemd restart, no unit edit.
   The requesting job counts as in flight, so a job that asks for its own
   restart finishes before the daemon goes down instead of deadlocking against
   it. A drain that does not finish within its bound aborts and lifts the
   cordon (never cancelling work implicitly); `--force` cancels the remaining
   jobs and restarts at once. An unchanged on-disk binary is refused as a
   no-op, the replacement is prechecked with `version`, the exec follows only a
   clean shutdown, and a second request while one is pending is a no-op. The
   unit's reload stays `ExecReload=~/.local/bin/exocomp --remote self reload`;
   the unit file itself is not edited in this repo.
3. **Job recovery (follow-up).** The interrupted listing becomes something an
   operator re-drives from, rather than a read-only post-reboot view.

What needs which action:

| Change | Action |
|---|---|
| Secrets/redaction, mutes, approvals — read from the DB per use | live, no action |
| Schedules — read once into memory | `exocomp reload` (or `SIGHUP`) |
| Binary change | `exocomp restart` (drains, then re-execs in place) |
| `serve` flag change | the unit's own restart (a re-exec keeps the same argv) |

The audit of which rows are read-once versus read-per-use stays a follow-up:
schedules are the only reloadable set named here.

## Consequences

- Schedule edits reach a running daemon without cancelling or retiring work.
- A restart stays available for what only a restart can change, and when it
  happens it happens on a drained daemon.
- `ExecReload` gives the unit a reload that is not a restart; the unit file
  stays Ansible-managed and out of this repo.
- A restart re-execs in place rather than through the unit, so it carries the
  same argv: a `serve` flag change is still the unit's own restart.
- Phase 3 (re-driving interrupted jobs) is the remaining follow-up; interrupted
  jobs keep today's listing until it lands.
