# ADR 0008 — Durable multi-step execution: step ledger, DBOS deferred

Status: accepted · 2026-08-22

## Context

Upcoming choreographies need crash-resumable multi-step execution with rollback
points: DEK rotation (the most dangerous operation in the system), upgrade-with-
snapshot, and multi-leg backup verification. Durable-execution engines exist for
exactly this. Assessment of the embeddable field (sources in the assessment note):

- **DBOS Transact** is the only candidate compatible with the invariants:
  in-process library, MIT, and — decisively — a SQLite system database that is now
  DBOS's **default** (Python docs, verified 2026-08-22; the Go SDK ships it backed
  by the same `modernc.org/sqlite` driver this project already uses, v1.0.0,
  2026-07-31). It genuinely embeds; no second process, no Postgres. DBOS's own
  production caveat for SQLite is multi-server distribution — a topology this
  project forbids by invariant, so the caveat does not apply here.
- Everything else disqualifies structurally: Restate (separate server, BSL),
  Inngest (server, source-available), resonate (separate server despite SQLite),
  Temporal (server cluster), River (Postgres). No mature Go-native SQLite-embedded
  durable-execution library exists — the gap was searched and is empty.

## Decision

**Adopt the pattern, not the engine.** Multi-step choreographies are built on a
small step-ledger extension of the existing job store: a `workflow_step` table
(workflow id, step, status, result, rollback marker) written in the same
transaction as each step's effect, idempotent steps, resume-from-ledger in the
boot sweep. The existing queue remains the only execution authority.

DBOS is **deferred, not rejected**: even as the vendor default, its Go SQLite path
is one month past GA, ~90% of commits are one author's, and adopting it would place a second workflow runtime —
its own schema, migrations, recovery semantics — beside a queue whose properties
(SQL-enforced concurrency keys, redaction at persistence, exit-code doctrine,
approval gating, crash sweeps) six milestones of gates have proven and no generic
engine provides.

## Consequences

We own ~200 more lines of sequencing code and its tests; rotation/upgrade land on
infrastructure the drills already exercise. Revisit DBOS in 6–12 months if its
SQLite backend matures, the contributor base broadens, and a choreography
genuinely outgrows the ledger (fan-out with per-step retries and timers would be
the signal).
