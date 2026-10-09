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
   it; `--wait` is the operator's form and the CLI refuses it inside a job for
   exactly that reason. A drain that does not finish within its bound aborts
   and lifts the cordon (never cancelling work implicitly); `--force` cancels
   the remaining jobs and restarts at once. An unchanged on-disk binary is
   refused as a no-op, the replacement is prechecked with `version`, the exec
   follows only a clean shutdown, the run-secret staging session directory is
   closed before the exec (an in-place exec keeps the pid, so the next start's
   liveness sweep would otherwise leave it), and a second request while one is
   pending is a no-op — the pending reservation is taken atomically, so two
   simultaneous requests cannot both run the precheck. The unit's reload stays
   `ExecReload=~/.local/bin/exocomp --remote self reload`; the unit file itself
   is not edited in this repo.
3. **Job recovery (landed).** The interrupted listing became something an
   operator re-drives from: `exocomp jobs redrive` selects the cut-off rows
   (default `--state interrupted`, repeatable with `cancelled`) and
   re-enqueues each as a fresh queued job carrying the same spec — rebuilt
   from the row's stored args, labelled with the `redrive` trigger — through
   the one `service.Reenqueue` path the web "Run again" uses. The idempotency
   and dedup keys are dropped, so the enqueue lands a new row rather than
   collapsing onto the original; each redrive instead lands under a
   deterministic key derived from the cut-off row's id, so a repeated redrive
   is a no-op that reports the original as already re-driven. A workflow-step
   row is excluded: the sequencer re-arms it from the ledger (ADR 0008), so
   re-driving it would run the step twice and drop any approval gate it
   carried. A real run needs confirmation (`--yes`, or an answered prompt;
   `--dry-run` enqueues nothing); a clean restart-after-drain interrupts
   nothing, so there is nothing to recover; and auto-redrive is deliberately
   off — a cut-off destructive run is the operator's call, never auto-rerun.
   No new reactive event: a restart is control-plane state, not a reactive
   trigger.

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
- Phase 3 (re-driving interrupted jobs) is landed as `exocomp jobs redrive`;
  interrupted jobs keep today's listing alongside it.
