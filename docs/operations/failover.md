# Cold-standby failover

The primary is down and you want a second host serving. This is the drilled procedure, and
the property that makes it safe to run in a hurry: **two hosts sharing one replicated store
can never both be active.** The [disaster-recovery](disaster-recovery.md) runbook rebuilds a
dead host from a backup target; this page is the narrower case where a **standby** takes over
a store both hosts can reach, and how the fence stops a split-brain when the primary is not as
dead as you thought.

## Why a lease, on top of the lock

`exocomp serve` has always held a host-local lock on `serve.lock` in the data dir (see
[running the daemon](serve.md)): two daemons on **one** host cannot both start. That lock is a
`flock` on a local file and a check that a local pid is alive — both meaningless across hosts.
Two daemons on two hosts, sharing one replicated `exocomp.db` (a Litestream copy, a shared
volume, a restic restore of the same snapshot), would each pass their own local lock and both
go active: firing every schedule twice, racing for every queued job, and — worst — each running
the boot sweep, whose pid-liveness is host-local and would retire the *other* host's in-flight
jobs as though their owner had died.

The fence closes that. A single-row **active lease** lives in the store itself, with a
wall-clock TTL. The active daemon holds it and renews it; a starting daemon that finds a **live**
lease held by another holder refuses to become active and names who holds it. Both guards stay:
the flock is the cheap same-host answer, the lease the cross-host one.

```
serve.lock (flock)   → one daemon per HOST
serve_lease (row)    → one active daemon across all hosts sharing the store
```

The lease is not a power controller. It prevents two daemons from both **activating against the
shared store**; it does not switch off a primary that is independently mutating external systems
out of band. Making sure the primary is truly stopped is still yours — the fence is what keeps a
mistake from corrupting the queue, not a substitute for confirming the mistake.

## Procedure

```mermaid
sequenceDiagram
    actor Op as Operator
    participant P as Primary (down)
    participant SB as Standby host
    participant S as Shared store

    Op->>P: confirm stopped (systemctl stop / powered off)
    Op->>SB: restic restore latest  (keyring + DB + secrets)
    opt seconds of RPO matter and Litestream was replicating
        Op->>SB: exocomp restore --from-litestream …
        SB->>SB: decrypt round-trip verifies the replica bytes
    end
    Op->>SB: exocomp serve
    SB->>S: acquire the active lease
    alt lease is free or stale
        S-->>SB: acquired — standby is now active
        SB->>SB: boot sweep + resume interrupted workflows
    else lease still LIVE (primary not actually dead)
        S-->>SB: REFUSED, names the holder + last renew
        Note over Op,SB: stop the primary for real, then retry;<br/>--force-lease only once it is gone
    end
```

1. **Confirm the primary is stopped.** `systemctl --user stop exocomp` on it, or confirm it is
   powered off / partitioned. This is the step the fence cannot do for you.

2. **Restore the store onto the standby** with the authoritative restic chain — it is the only
   one that carries the keyring and sealed secrets:

   ```sh
   exocomp restore --from <kind:uri> --dest <data dir>
   ```

   It refuses a destination that already holds a keyring, so a standby that was itself once a
   primary needs a fresh data dir.

3. **Optionally fast-forward the database** to its freshest state, *if* the Litestream sidecar
   was replicating and seconds of lost job history matter. This recovers `exocomp.db` **only**,
   onto the data dir the restic restore just wrote (it needs the `keyring.age` beside it), and
   it does not trust the replica's unencrypted bytes until a decrypt round-trip proves them:

   ```sh
   exocomp restore --from-litestream --dest <data dir> --identity recovery.key \
     --replica <replica url>        # omitted: read from the litestream.replica_url setting
   ```

   A database that fails the round-trip is discarded and the restic-restored one is left in
   place. See [disaster-recovery](disaster-recovery.md#fast-rpo-restore-litestream) for the
   precedence and the plaintext-remote caveat.

4. **Start the daemon — deliberately.** Neither restore resumes anything; starting `serve` is
   the act that makes this host active:

   ```sh
   exocomp serve
   ```

   On startup it acquires the active lease **before** touching a single job row. Three outcomes:

   - **Free or stale lease** → acquired. The standby is now the one active daemon. It prints
     `active lease acquired (holder …)` and proceeds.
   - **Live lease held by another holder** → it **refuses** and names the holder and when the
     lease was last renewed. This is the fence catching a primary that is not actually dead (or
     a second operator racing you). Go back to step 1. Do not reach for `--force-lease` to make
     the message go away.
   - You are certain the named holder is gone (crashed without releasing, TTL not yet lapsed)
     and cannot wait out the TTL → `exocomp serve --force-lease` takes it over. Use this only
     when the holder truly cannot be running, because it is the one path that overrides the
     split-brain guard.

5. **Interrupted work re-drives itself, mostly.** The boot sweep retires the rows the dead
   primary left `queued`/`running` to `interrupted`, and the workflow sequencer's first pass
   resumes any multi-step workflow the ledger left mid-flight (an apply→play handoff, a
   provision chain) from where it stopped — a succeeded step is never re-run. A **standalone**
   one-shot job that was interrupted is terminal and is not auto-retried; re-issue it yourself
   (`exocomp workspace apply …`, `exocomp playbook run …`) once you have confirmed what the
   primary did and did not finish. Verify with `exocomp tui` and `exocomp doctor`.

## A resurrected primary

If the old primary comes back — a reboot, a healed partition — while the standby is active, it
tries to renew or re-acquire the lease and finds the standby holding a live one. A **starting**
primary refuses exactly as in step 4. A primary that was mid-renew when it was fenced (you ran
`--force-lease`) discovers on its next renew that the lease is no longer its own and **stands
down**, running the same graceful stop a `SIGTERM` triggers. Either way, only one daemon stays
active against the store.

The TTL is the window this costs you: after an **ungraceful** primary loss the standby must wait
for the lease to go stale before it can acquire without `--force-lease` (default TTL 30s, tunable
with `--lease-ttl` / `--lease-renew`). A **clean** `serve` shutdown releases the lease on the way
out, so a planned failover has no wait at all.

## Drilling it

Rehearse against a throwaway copy so the first real run is not the first run:

- Restore last night's snapshot into a scratch data dir (`exocomp restore --from … --dest /tmp/standby`).
- With no daemon holding the store's lease, `exocomp serve` there acquires cleanly — the free-lease path.
- To rehearse the **refusal**, point a second `serve` at the *same* data dir: the host-local flock
  refuses it first. To rehearse the cross-host refusal specifically, the fence's behaviour is
  covered by the daemon gate tests (`cmd/exocomp`), which stand two holders against one store and
  assert the second refuses, claims nothing, and only takes over once the first releases or its
  lease goes stale.

This complements the [`backup drill`](disaster-recovery.md#verification--the-part-people-skip),
which proves the *restore* half; the failover drill proves the *activation* half.
