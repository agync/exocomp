---
type: Decision
title: "Decision 0018 — Opt-in aggregate check events with governed `--limit-hosts`"
description: "Opt-in aggregate check events carrying the present host set, superseded by Decision 0019 which removes them."
status: deprecated
tags: [decision, checks, superseded]
---

# Decision 0018 — Opt-in aggregate check events with governed `--limit-hosts`

Status: accepted · 2026-09-15

## Context

Per-target check events cannot express "run once over the hosts that are
present": the headline case needs a set, not one event per host. The design
deferred a set-event because expanding an event into a run's `--limit` would let
attacker-influenced text shape argv — the trusted-source line
([design/check-triggered-runs.md](../design/check-triggered-runs.md) §3.3). A
webhook body lands in a durable table and must never reach ansible argv; a
sweep-produced event is written by the daemon's own probe code.

## Decision

A check may opt into an aggregate event (`check_row.aggregate`, default off).
When enabled, the sweep reads the present set (targets currently up) against a
threshold (`min_up`, 0 = all), and on a declared transition emits one event
keyed `check:<check>:aggregate` carrying `Hosts` — the sorted present target
names. The rollup's own hysteresis state lives in `check_state` under the
reserved target name `*`, which is not a valid target name and is filtered
everywhere a state row is read as a target.

`reactive.Event` gains `Hosts []string` and exposes `event.hosts` to CEL. The
field is populated **only** by the sweep; every webhook normalizer leaves it
empty, and a test pins that.

A `playbook` rule action may set `--limit-hosts` only when its source filter is
`check`. At fire time the limit is derived from `event.Hosts` under four
refusals: a non-check source, an empty host set, an invalid name, and a host not
declared as a target of the check the event names. The last two are what keep
the expansion inside the daemon-controlled set, so no event value reaches argv
unvalidated. A rule without `--limit-hosts` keeps its fixed, rule-add-time
`--limit`.

## Consequences

- The feature is default-off; a check that does not set `aggregate` behaves
  exactly as before, and no existing rule changes.
- The trusted-source line is enforced at the spec builder, not by convention:
  only daemon-produced sweep events can carry a host set, and only the named
  check's own targets can appear in the derived limit.
- A new dedup/cooldown key (`check:<check>:aggregate`) sits alongside the
  per-target keys; the per-source cooldown split still governs.
- **Unproven until dogfooded.** The "run over the present subset" need is the
  least-evidenced part of the design; if the operator's real work is already
  served by per-target rules, the feature is removed rather than carried
  (the plan's S9 kill criterion).
