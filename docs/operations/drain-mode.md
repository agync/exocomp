---
type: Guide
title: "Draining for a planned reboot"
description: "Cordoning a running daemon so it fires no schedules and claims no new jobs while in-flight work finishes, then polling whether it is safe to reboot."
status: stable
tags: [operations, serve, reboot]
---

# Draining for a planned reboot

A kernel update or hardware maintenance means rebooting the control node. `exocomp serve`
already drains on shutdown — it stops the scheduler, gives in-flight jobs a grace window,
then cancels whatever is left ([Running the daemon](serve.md)). But that drain is a
side-effect of the process exiting, and the grace window *cancels* work it does not like.
For a *planned* reboot you want the opposite: cordon the running daemon ahead of time, let
in-flight work finish on its own clock, and be told when it is safe to reboot.

`exocomp drain` is that action. It is one operator on one node ([invariant
1](https://github.com/agync/exocomp/blob/main/AGENTS.md)); there is no failover partner and none is needed — this is planned
maintenance, not a crash.

## What a cordon does

A cordoned daemon:

- **fires no new schedules** — the scheduler is paused, not torn down. A cron occurrence
  that passes while cordoned is *not lost*: because a suppressed fire never stamps
  `last_run`, lifting the cordon runs the same catch-up a daemon does after downtime, so the
  missed occurrence fires once (coalesced), never once per missed tick.
- **claims no new jobs** — a row that reaches the queue while cordoned stays `queued`. It is
  claimed once the cordon lifts, or swept to `interrupted` on the next boot if the reboot
  happens first.
- **lets in-flight jobs finish** — a job already running is *never* cancelled by a cordon.
  This is the key difference from shutdown's grace window. A backup leg writing its
  `backup_run` row, a tofu apply pushing state — all run to their own completion.

"Safe to reboot" is the conjunction the CLI computes for you: **cordoned AND nothing still
in flight**. A queued-but-unclaimed job does not block a reboot (it is swept to
`interrupted` and re-drivable); a *running* job does.

## Driving it

`drain` controls a **running** daemon, so it goes over the operator API, the same channel
`--remote` uses for every other write ([Driving a remote daemon](remote-access.md)). It needs
a selected remote — the control node targets its own daemon:

```
exocomp --remote self drain
cordoned: yes  in-flight: 2  safe to reboot: not yet
```

Cordon and wait for the two in-flight jobs to finish, then reboot:

```
exocomp --remote self drain --wait
waiting: 2 job(s) in flight
waiting: 1 job(s) in flight
cordoned: yes  in-flight: 0  safe to reboot: yes
```

`--wait` polls until safe or `--timeout` (default 10m) elapses; a timeout exits non-zero, so
a reboot script does not proceed on a daemon still finishing work. `--status` reports
readiness without cordoning. Lift the cordon (a reboot postponed, say) with `--resume`:

If the goal is to restart the daemon itself rather than reboot the host,
[`exocomp restart`](serve.md#restarting-the-daemon) does the same drain and then
re-execs the on-disk binary in place, so no reboot and no systemd restart is
involved.

```
exocomp --remote self drain --resume
cordon lifted; the daemon is taking work again
```

Every verb honours `--output json`; the readiness envelope is `maintenance.readiness`, and a
script branches on its `safe_to_reboot` field:

```
exocomp --remote self drain --output json
{"schema_version":1,"kind":"maintenance.readiness","data":{"cordoned":true,"in_flight":0,"safe_to_reboot":true}}
```

A cordon is in-memory daemon state: a reboot clears it, so the daemon comes back up taking
work normally. There is nothing to un-set after the reboot.

## After the reboot: interrupted jobs

A reboot cuts off whatever was still running, and sweeps whatever was still queued. Both land
in `interrupted` — a terminal state that is **never auto-retried** ([the sweep](serve.md)).
List them to see what to re-drive:

```
exocomp --remote self drain interrupted
ID  STATE        RUNBOOK   KIND    TRIGGER   EXIT  QUEUED
41  interrupted  nightly   backup  schedule  -     2026-08-22 03:00:00Z
```

`drain interrupted` reads them newest-first. It works locally too (reading the store
directly), for the common case of an operator on the freshly-rebooted box. Re-drive the ones
that still matter with `exocomp jobs redrive` (dry-run first, then `--yes`);
nothing re-runs on its own. See [Recovering jobs a restart cut
off](serve.md#recovering-jobs-a-restart-cut-off).

## The mechanism, and why not a signal

`SIGHUP` reloads schedules and `SIGTERM` drains-and-stops, but a signal cannot carry the
in-flight count back, so it cannot answer "safe yet?". The cordon is therefore an
operator-scoped API action on the Decision 0009 listener, gated exactly like `plan`/`apply`: a
read-only token is refused, and a browser session may not drive it. It is deliberately *not*
a queue job — a cordon is control-plane state, not a run, so it has no ledger entry.
