---
type: Decision
title: "Decision 0019 — Aggregate check events removed"
description: "Aggregate check events, the event host set and the limit-hosts flag are removed, leaving migration 0041 as inert schema."
status: stable
tags: [decision, checks]
---

# Decision 0019 — Aggregate check events removed

Status: accepted · 2026-09-26 · supersedes [Decision 0018](0018-aggregate-check-events.md)

## Context

Decision 0018 introduced an opt-in check-level aggregate: a check could emit one
event over the targets currently up, carrying `Hosts`, and a `playbook` rule
could derive its ansible `--limit` from that set via `--limit-hosts`. The record
itself flagged the capability as unproven and named its own kill criterion
(S9): if the operator's real work is served by per-target rules, remove it
rather than carry it.

The operator's work is served by per-target rules. Per-host events keep one
condition, one dedup key, and one run per target, which is simpler to reason
about and debug than a set event whose hysteresis state lives under a reserved
`check_state` target named `*` and whose `--limit` derivation needs its own
trusted-source validation.

## Decision

Remove the aggregate check feature entirely:

- `check_row.aggregate`/`min_up` config and `SetCheckAggregate`; the CLI
  `--aggregate`/`--min-up`, MCP `aggregate`/`min_up`, API body fields, and the
  `/checks` dialog controls.
- The sweep's aggregate rollup and its reserved `check_state` row named `*`.
- `reactive.Event.Hosts` and the CEL `event.hosts`.
- The `playbook` action's `--limit-hosts` and `RuleAction.LimitHosts`, together
  with the inventory-name validation the derived limit required.

Migration `0041_check_aggregate.sql` and its two columns stay as inert schema.
A forward-only migration is never deleted: a database already at
`user_version` 41 would otherwise be unreadable by the binary. No drop-column
migration is added.

## Consequences

- Checks emit per-target events only; one target is one condition, one dedup
  key, and one cooldown slot, with no reserved target name to filter on every
  read path.
- The trusted-source line that guarded `event.hosts` and the derived `--limit`
  is moot and is removed with the feature; a webhook body can no longer reach
  ansible argv because there is no expansion path left to reach.
- A `playbook` rule requires its fixed `--limit` again, unchanged in meaning
  from before Decision 0018.
- `check_row` keeps two unused columns. That is deliberate, recorded debt: they
  are inert, defaulted, and never read or written by the binary.
- `TestMigration0041UpgradesFrom040` no longer asserts the aggregate config; it
  proves the in-place upgrade and that a pre-existing check row survives it.
