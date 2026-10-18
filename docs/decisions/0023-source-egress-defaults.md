---
type: Decision
title: "Decision 0023 — Source-scoped default egress ports for ansible"
description: "Persist an operator-set egress set on a source and union it into every ansible run that names it, at queue admission; repo content can never declare a port."
status: stable
tags: [decision, sandbox, egress, ansible, queue]
---

# Decision 0023 — Source-scoped default egress ports for ansible

Status: accepted · 2026-10-18

## Context

An ansible job's sandbox grants TCP 22 and nothing else
(`ProfileFor(KindAnsible)`). A control-plane play that calls an API over HTTPS
needs 443, declared per run with `--egress`. An operator retypes that declaration
on every manual run, and the sandbox posture is a per-run property, so there is
nowhere to record "this source's plays talk to an API".

OpenTofu already persists the equivalent per workspace: `workspace.extra_egress_ports`
(migration 0021) is an operator-set port set that widens a workspace's tofu runs.
Ansible has no persisted equivalent. The gap was already anticipated in the web
layer: `source_detail.go` documents a "source default" (a SPEC §5.2 phrase) that
no field backs, and the launcher currently proxies it with the last run's ports.

The question this record settles is whether to persist a per-source default, where
it is applied, and what constrains it — in particular whether a source's content
could ever declare its own egress.

## Decision

**Persist an operator-set default egress set per source, and union it into every
ansible run that names that source.**

- The set lives on the `source` row (`source.extra_egress_ports`, new migration
  `0043_source_egress.sql`, mirroring 0021's DDL). It is written only through an
  operator-scope face, like the source's other fields.
- Effective ansible egress is the kind default (22) **∪** the source default
  **∪** whatever the run declares (`--egress` / the job's `egress_ports`).
  Additive, de-duplicated, sorted. A run widens, never narrows: a declared
  `--egress` cannot reduce the default.
- The union is applied at **queue admission** (`Enqueue`, `EnqueueGated`) and
  nowhere else, gated on `Kind == KindAnsible && Source != ""`. Those two calls
  are the only places a job row is created, so every producer inherits the default
  uniformly and the recorded row is the grant the run actually receives.
- **A source's content can never declare a port.** The default comes only from the
  store. No checkout file, `.exocomp/hooks.yml`, inventory, extra-var, argv token,
  or event payload contributes a port. A repo that could grant itself egress would
  escape the sandbox the per-job profile exists to enforce.
- A source-default widening is a `--privileged`-class grant: operator scope only,
  never a readonly/approve token, an agent tool, or a host-token scope, and
  audited as an actor event.

Rejected: applying the union in `service.RunPlaybook` (it reaches only one of the
seven producers that build an ansible spec); applying it in the executor driver
(the row would then understate the grant and every surface that shows the run's
egress would lie, because only the row is durably re-derivable).

## Consequences

- The job row records the effective set, so the run-config tile and the source's
  last-run posture show the grant the run actually got, not just what the caller
  typed.
- The grant follows the source, and a source is named by more than manual runs:
  host-token requests, webhook-triggered rules, and rerun/redrive all build a
  spec with that source. Those runs inherit the default. Surfaces that show a
  rule's or a run's reach — the rules page and the approval page — must therefore
  render the effective set, or a published bound (a rule's declared ports) stops
  describing what an unattended run may open.
- A scheduled play carries no source (`ScheduleArgs` has no `Source`), so a
  source-keyed default does not reach it. Giving a schedule a source is a separate
  change, and this record does not claim the default covers scheduled plays.
- The OpenTofu precedent is deliberately **not** followed at the application
  point. Tofu reads `workspace.extra_egress_ports` in the driver at run time and
  never writes it to the row; ansible records the merged grant instead. The shared
  idea is only "operator-set, persisted per scope"; the ansible design is the
  stricter one.
- The egress residual is unchanged in kind: Landlock filters by port, not
  destination, and this default is a persisted port grant, so it can name the
  daemon's own listeners. That reach is closed on the credential side, as the
  threat model records.
- Work this decision depends on, but does not itself perform: `rearmStep`
  (workflow resume) must carry `Source` and `EgressPorts` so a resumed step keeps
  the grant; the approval page must render the effective set; the wire source
  projection must carry the field. These are prerequisites, not consequences of
  the design.
- Not yet implemented. The operational pages (`operations/serve.md`,
  `guide/ansible.md`) and the code land separately, in the same commit series as
  each other.
