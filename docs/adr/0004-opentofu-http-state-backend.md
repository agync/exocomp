# ADR 0004 — OpenTofu state: HTTP backend implemented in SQLite

## Status

Accepted · 2026-08-21

## Context

The Terraform Cloud half of the brief needs remote state, locking, version history, and
plan-gated applies. The obvious answer — an S3 bucket with `use_lockfile` — quietly assumes
a paid cloud provider:

- MinIO's conditional-write implementation rejects the `*` wildcard etag that
  `use_lockfile` requires (minio#20346).
- Garage has no conditional PUT at all.
- RustFS has ETag-quoting bugs.

Those are precisely the S3-compatible stores a homelab runs. Meanwhile no packaged TACoS
platform fits the scale (Terrakube is a Java multi-service stack, Burrito and
tofu-controller need Kubernetes, OTF is a pre-1.0 solo project, Atlantis is PR-driven CI
glue), and Semaphore UI paywalls Terraform state behind its Pro tier.

## Decision

`exocompd` implements the OpenTofu HTTP state backend protocol itself, against its own
SQLite database. Roughly 400 LOC; the hard part (encryption) is done by `tofu`.

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/api/v1/tf/state/{workspace}` | latest `tfstate.ciphertext`, or 404 → tofu treats as empty |
| `POST` | `/api/v1/tf/state/{workspace}?ID={lock}` | insert a new row at `serial+1`, verifying the lock holder |
| `LOCK` | `/api/v1/tf/state/{workspace}` | `INSERT INTO tflock`; a UNIQUE primary key gives atomic locking inside SQLite's own transaction, 423 plus the lock JSON on conflict |
| `UNLOCK` | same | delete, checking `lock_id` |

Backend configuration is generated per run and points at `127.0.0.1`, authenticated by a
bearer derived per workspace from the DEK: `base64(Derive("tf-backend", workspace))`,
recomputed by whoever needs it rather than stored anywhere. Applies are *additionally*
serialized by the queue's `concurrency_key`, so a single control plane never runs two
applies against one workspace regardless of what any storage backend can promise.

State remains encrypted by OpenTofu itself: `exocompd` sets `TF_ENCRYPTION` with the
`pbkdf2` key provider and a passphrase derived per workspace from the DEK
([ADR 0003](0003-secrets-envelope.md)), so there is no extra secret to lose and no third
party in the DR path.

## Consequences

- Locking is correct on any storage, including a NAS mount and the S3-compatible stores
  above, because the lock is a row rather than an object-store race.
- Every serial is retained, which yields state version history and rollback as ordinary UI
  objects. The pruning policy — the last 100 plus everything from the last 30 days — is
  designed and not yet implemented.
- Stale-lock recovery is an audited one-click `force-unlock` instead of a console trip. On
  boot, `tflock` rows whose owning job is dead are flagged for it.
- Drift becomes a first-class object: a scheduled `tofu plan -detailed-exitcode`, exit 2 →
  a plan-outcome row in the job's append-only `job_event` log → notify and optionally open
  an approval-gated apply. The job itself succeeds, because changes being present is a fact
  about infrastructure rather than a failed run.
- A saved plan file is a credential. It carries the backend address and the bearer it was
  made against, so plan artifacts live inside the 0700 data dir, and the backend address
  has to be the same one when a later job applies the plan. The running backend records
  its address in the data dir; a stale record is re-bound rather than trusted.
- The claim "state is opaque to the control plane" is **false** and is not used to justify
  anything. `exocompd` derives the passphrase. The ciphertext is opaque to backup targets.
- The protocol surface is wider than four handlers: `DELETE`, the custom `LOCK`/`UNLOCK`
  verbs that some proxies drop, and the `-migrate-state` interaction. Taking over existing
  state is therefore a documented, reversible adoption runbook tested against fixtures —
  `tofu init -migrate-state` onto the HTTP backend with a back-out while the old backend
  still exists.
