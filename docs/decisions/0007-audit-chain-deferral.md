---
type: Decision
title: "Decision 0007 — Audit hash chain: deferred, with a revival condition"
description: "The audit hash chain is deferred, and reviving it requires an off-host-anchored head."
status: stable
tags: [decision, security, deferred]
---

# Decision 0007 — Audit hash chain: deferred, with a revival condition

Status: accepted · 2026-08-21

## Context

The rejected hybrid candidate carried a hash-chained, tamper-evident audit table with
restore-time chain verification. The accepted thin-core design dropped it without
recording the decision — a gap in the decision record. The substance: anyone able
to rewrite `job_event`/audit rows owns the writing
uid and can rewrite an in-database chain head too, so an unanchored chain adds cost
without adding evidence.

## Decision

Defer the audit hash chain. Revive it only in the form that adds real evidence: each
audit row carries the previous row's hash, and the chain head is exported off-host
with every backup (committed alongside `keyring.age` in the git leg, included in the
manifest the restore verifies). Until then, the append-only trigger on `job_event`
and the immutable off-host backup copies are the tamper story, stated honestly.

## Consequences

History can be rewritten by the daemon's uid without detection until the anchored
chain lands (backlogged v1.1). Restores can only prove integrity of what the backup
carried, not continuity between backups. The threat model must state both.
