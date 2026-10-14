---
type: Decision
title: "Decision 0001 — Thin-core architecture"
description: "Thin-core architecture: one Go binary owning state, sequencing, keys and recovery, with every other tool kept as a pinned subprocess."
status: stable
tags: [decision, architecture]
---

# Decision 0001 — Thin-core architecture

## Status

Accepted · 2026-08-21

## Context

The brief is six jobs at homelab scale (one operator, a handful of hosts), each of which
must survive two hard constraints — trivial bootstrap and trivial disaster recovery. See
[requirements](../design/requirements.md).

Three architectures were designed independently and then reviewed adversarially; the
review found 30 enumerated gaps (sections A–H). The critique that produced them is
archived in the exocomp design phase of the lab wiki, outside this bundle.

- **compose-first** — Semaphore UI as the execution engine, a `ctl` companion binary for
  glue.
- **hybrid** — Semaphore UI plus a `keel` supervisor that owns keys and backup.
- **thin-core** — one Go binary owning state, sequencing, keys, and recovery; every tool
  that already exists (`ansible-core`, `opentofu`, `restic`, `sops`, `age`, `git`) is a
  pinned subprocess.

The comparison was decided on *structural* flaws — ones that cannot be patched without
abandoning the premise of the design.

| | compose-first | hybrid | thin-core |
|---|---|---|---|
| Guardrails (approvals, cooldowns, caps) | bypassable — Semaphore's own scheduler and Run button dispatch jobs the gate never sees | bypassable — same hole | enforced in the only queue that exists |
| Secret redaction in job logs | none; Semaphore's log capture persists plaintext, which then flows into backups | none | redaction filter + known-secret scan, fuzzed |
| Auth | two login systems, two session cookies, one origin | same | one identity source |
| Backup consistency | backup runs *as a Semaphore task*, so the DB is never quiescent at snapshot time | quiesce window exists, but two state stores with no shared transaction | one SQLite file, `VACUUM INTO` |
| Key rotation | — | silently destroys Semaphore's key store (rotates the derived encryption key without re-encrypting the store) | sequenced single job with a rollback point |
| Effort to v1 | ~42 person-days | ~46–63 person-days | ~22 FTE-weeks |

Effort figures carry the candidates' own units and scopes and are not directly
comparable — the critique flags exactly that; the decision rests on the structural
rows above, not this one.

Compose-first and hybrid are cheaper only until the bypass hole is closed: doing so means
disabling Semaphore's scheduler, blocking its Run button, and restricting task creation to
the companion's token — at which point the project has paid for Semaphore and discarded
most of what it does.

## Decision

Build thin-core. One static Go binary (`exocompd`) owns *state, sequencing, keys, and
recovery* and nothing else; it ships inside one rootless container with pinned copies of
the tools that do the actual work as subprocesses.

The single hard rule: the core never reimplements a tool that exists. It contains no
backup engine, no Terraform executor, no Ansible executor, no rules DSL of its own
invention, and no database server.

The detail-level flaws the review found in thin-core are closed by the
[amendments](../design/amendments.md), which are normative over the candidate document.

## Consequences

- The guardrails, the redaction filter, and the recovery chain are enforceable because one
  queue is the only execution path. This is the property the whole design is bought for.
- ~22 FTE-weeks of work and solo-maintainer bus factor are accepted costs.
- The bus factor is bounded by an invariant, not a promise: **nothing is stored in a
  bespoke format** — plain SQLite, plain restic repos, plain git bundles, plain age files,
  standard OpenTofu state. The exit cost from abandonment is a weekend of scripting, not
  data loss. `exocomp export --portable` (Semaphore-importable) is the explicit escape
  hatch.
- Semaphore UI stays a data-model reference and an export target, never a dependency.
- Because `ansible-core` is Python, the deployed artifact is honestly an *image* with one
  static binary inside it, not a single binary.
- Subprocess correctness is decided by exit codes only; parsed output enriches the UI, so
  an upstream `--json` schema change degrades the display, not the result.
