---
type: Decision
title: "ADR 0017 — One probe vocabulary for checks and gates"
description: "One shared probe vocabulary for checks and gates, with the SSRF guard applied per face and Location opt-in per kind."
status: stable
tags: [decision, checks, probes]
---

# ADR 0017 — One probe vocabulary for checks and gates

Status: accepted · 2026-09-15

## Context

Reactive checks (`tcp`, `ping`, `http`) and post-run gate probes (`http`, `dns`,
`tls`, `metric`) were two disjoint implementations of the same idea: run a
network assertion, decide up/down from dial/exit/status truth. The overlap was
real and drifting — both implemented HTTP with different default accept ranges
(checks 2xx/3xx, gates 2xx) and only checks carried a dial-time SSRF guard. The
v1.1 roadmap's cert-expiry signal (R2) was already written in the gate package
but unreachable from a check, so an operator needed a second monitor for it.

## Decision

Introduce `internal/probe`: one union `Spec` (`Kind` plus the per-kind fields),
one `Validate`, one `Run` returning up/down + duration + detail, and one
`Location() (netip.Addr, bool)` accessor. Both checks and gates build a `Spec`
and call it; `verify` keeps its public `Probe`/`Encode`/`Decode` and job routing
unchanged. Checks gain `dns`, `tls` (not-after floor), and `metric`
(op/value/threshold) as a pure Go addition once the kind vocabulary exists.

Two consequences are explicit, not incidental:

- **`check_row.kind` widens by rebuilding both `check_row` and `check_state`** —
  SQLite cannot alter a CHECK, and `check_state`'s incoming FK means the child
  is dropped before the parent. Targets stay a flat, additive JSON shape so
  existing rows and golden tests are not restructured.
- **The SSRF guard is an option.** Checks refuse loopback/link-local/unspecified/
  multicast (`AllowLoopback=false`); gates allow them (`true`), because a gate
  legitimately probes a loopback health endpoint that a check must not.
- **Location is opt-in per kind.** Only `tcp`/`ping` address literals (and a
  bare-IP `tls` address) classify into a netgroup; `dns`/`tls`/`metric` carry
  none, and every netgroup read path goes through `Location` rather than
  assuming `Address` is an IP.

## Consequences

- `dns`/`tls`/`metric` reach a check for free; the roadmap's cert ladder and
  metric thresholds need no new engine.
- One HTTP implementation, one validation path, one test surface; the two
  accept-range defaults are preserved as a field and pinned by tests rather than
  silently unified.
- `internal/probe` does not import `store`; the store builds a `Spec` from a
  check kind + target, and validation delegates to the same `Spec.Validate`.
- A future probe kind is a Go addition plus, only if it changes the stored
  `kind` set, one widening migration — the same shape this ADR already paid for.
