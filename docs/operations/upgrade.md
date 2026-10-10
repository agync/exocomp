---
type: Runbook
title: "Upgrading and rolling back the binary"
description: "Upgrade and rollback as a ledger choreography of snapshot, stage, verify, swap and health-check, rolling back to the snapshot and prior binary on any failed step, with no sudo."
status: stable
tags: [operations, serve, rollback]
---

# Upgrading and rolling back the binary

`exocomp upgrade` swaps in a new `exocomp` binary as a step-ledger choreography
(ADR 0008): every step is one row in the `workflow_step` ledger and one job
through the queue, and any failed step rolls the whole thing back, leaving the
system exactly as it was. `exocomp rollback` is the manual escape hatch that
reverts the most recent upgrade on demand.

Neither command ever runs sudo. Both operate only inside the data dir and the
binary's own directory. If that directory is not writable by the user running
exocomp, upgrade refuses and prints the manual `install` line — it does not
reach for privilege.

## Upgrading

```
exocomp upgrade /path/to/new/exocomp
```

The five steps, in order:

1. **snapshot** — a vacuumed, self-verifying copy of the database is written
   under `<data-dir>/upgrade/<id>/snapshot`. This is the rollback point; its path
   is recorded as the step's ledger rollback marker.
2. **stage** — the new binary is copied *beside* the live one, on the same
   filesystem, without touching the live one yet.
3. **verify** — the staged binary is run for `version` and `doctor` against a
   throwaway data dir. Both must succeed and the version must be non-empty; a
   binary that cannot report a version or pass its own doctor is never swapped in.
4. **swap** — the live binary is preserved (recorded as the swap step's rollback
   marker), then the staged binary is atomically renamed into its place. This is
   the last mutating step: the rename replaces the directory entry while the
   running process keeps the old inode, so overwriting a live executable is safe.
5. **health** — the just-swapped binary is run for `version` and `doctor`
   against the real data dir — the post-swap check that it works with real state.

If every step succeeds, the choreography prints the ledger and the new version,
and keeps the snapshot and preserved prior binary as the point a later
`rollback` returns to. If any step fails, the rest are left `blocked` and the
rollback path restores the prior binary (if the swap had happened) and the data
snapshot, then reports which step failed.

The swap takes effect on the **next process start**. exocomp does not restart
itself in v1; restart the service to run on the new binary:

```
systemctl --user restart exocomp
```

Stop a running daemon before upgrading. A live daemon holds the binary as its
process image and drains the same job table the workflow enqueues into; upgrade
refuses while one is serving the data dir. The service restart after the swap is
the intended handoff.

### Crash safety

The swap is ordered last among the mutating steps, so:

- a crash **before** the swap leaves the old binary live and nothing to undo;
- a crash **after** the swap but before health is recoverable: the snapshot and
  the preserved prior binary are on disk, and `exocomp rollback` restores them.

Because each step's ledger row is a durable record and a succeeded step is never
re-run, the workflow is resumable from the ledger.

## Rolling back

```
exocomp rollback
```

This finds the most recent upgrade that actually took effect — one that swapped
a binary in and left a preserved prior — restores that prior binary and the
pre-upgrade data snapshot, and prints the ledger of the upgrade it reversed. A
failed upgrade that already self-reverted leaves no prior binary and is skipped:
there is nothing there to undo. As with upgrade, stop a running daemon first and
restart the service afterwards to run on the restored binary.

Rollback restores the pre-upgrade **data snapshot**, so it also discards changes
to the database made since that upgrade — jobs, backups, workspaces, sealed
secrets added after the swap. It is the escape hatch to run **promptly** after a
bad upgrade, before much new state accrues, not a general time machine. To keep
the new state and only revert the binary, install the prior binary by hand
(`install -m0755 <prior> <live>`) instead.

## Where the rollback points live

Each upgrade gets a directory under `<data-dir>/upgrade/` named by its workflow
id (a timestamp, so they sort chronologically). It holds the snapshot and the
preserved prior binary. The most recent few are kept; older ones are pruned on a
successful upgrade, since each holds a full copy of a binary.
