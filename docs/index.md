---
okf_version: "0.2"
title: exocomp knowledge base
description: Architecture, decisions, design, operations, and provenance for exocomp — a single-container homelab control plane.
---

# exocomp knowledge base

**exocomp** is a homelab control plane: one rootless container holding one Go binary
(`exocompd`) that owns state, sequencing, keys, and recovery, shelling out to pinned tools
(`ansible-core`, `opentofu`, `restic`, `sops`, `age`, `git`) for everything else.

Scale is one operator and a handful of hosts. Two constraints rule every decision here:
**trivial bootstrap** and **trivial disaster recovery**.

Start with [design/requirements.md](design/requirements.md) for what the project is for,
[architecture/overview.md](architecture/overview.md) for how it is put together, or
[operations/disaster-recovery.md](operations/disaster-recovery.md) for the property the
whole thing is bought for.

## Decisions

Foundational decisions — [adr/index.md](adr/index.md) holds the full table

| ADR | Decision |
|---|---|
| [0001](adr/0001-thin-core-architecture.md) | Thin-core architecture over compose-first and hybrid: guardrails, redaction, and the recovery chain are only enforceable if one queue is the only execution path |
| [0002](adr/0002-project-name-exocomp.md) | The name **exocomp** on `exocomp.dev`, after Control D's `ctrld` ruled out the working name |
| [0003](adr/0003-secrets-envelope.md) | age recipients → DEK → HKDF subkeys, so one printed secret recovers everything — with the read-only storage credential on the kit |
| [0004](adr/0004-opentofu-http-state-backend.md) | OpenTofu's HTTP state backend implemented in SQLite, sidestepping the MinIO/Garage conditional-write gap and giving state version history for free |
| [0005](adr/0005-job-sandboxing-nono.md) | nono's `enforcing` tier as the Linux default; every job is denied the database, the keys, and every powerful local socket |
| [0006](adr/0006-uv-toolchain-escape-hatch.md) | uv builds the Python layer, and `UV_TOOL_DIR` in the data volume turns that into a doctor-gated operator escape hatch |

## Architecture

How it is assembled, and which support claims are contractual —
[architecture/index.md](architecture/index.md)

| Page | Covers |
|---|---|
| [Overview](architecture/overview.md) | Component map, the one-queue convergence property, state layout, standing commitments |
| [Platform contract](architecture/platform-contract.md) | Tiers for systemd/Quadlet, rootless podman, docker-compose, and SELinux enforcing; the specified remote-access path |
| [Execution](architecture/execution.md) | `ExecutorDriver` tiers `disabled` / `enforcing` / `container`, offline-restorable job dependencies, boot-time reconciliation |
| [Reactive layer](architecture/reactive.md) | Webhook ingress, one event shape, CEL rules, the guardrails in ledger order, approvals, drift as an event, check probes with netgroup-classified peers |

## Design

Purpose, revision under review, and build order — [design/index.md](design/index.md)

| Page | Covers |
|---|---|
| [Requirements](design/requirements.md) | Two hard constraints, six goals, the anti-scope contract, and what is deferred behind a named seam |
| [Amendments](design/amendments.md) | The twelve normative changes from the 30-gap adversarial review |
| [Delivery](design/delivery.md) | v0.1 / v0.5 / v1.0 with effort, and the risks carried into each |
| [Design invariants](design/INVARIANTS.md) | The design philosophy, hard rules, and component vocabulary that survive any redesign |

## Operations

Standing it up and getting it back — [operations/index.md](operations/index.md)

| Page | Covers |
|---|---|
| [Bootstrap](operations/bootstrap.md) | `install.sh`, the six-screen idempotent wizard, node-zero self-management, adoption runbooks |
| [Containers](operations/containers.md) | The base image and what it carries, running it as shipped, inheriting to add system packages or Python dependencies, offline and multi-arch builds, and capturing an operator's image for recovery |
| [Running the daemon](operations/serve.md) | `exocomp serve` flags, the signal contract, the single-instance lock, and the daemon-aware CLI |
| [Draining for a planned reboot](operations/drain-mode.md) | `exocomp drain` — cordon a running daemon, let in-flight work finish, poll "safe to reboot?", and list interrupted jobs afterwards |
| [Disaster recovery](operations/disaster-recovery.md) | The three backup legs, the recovery chain, the dead-host runbook, the recovery card, failure modes, RPO/RTO, verification |
| [Offline caches](operations/offline-caches.md) | Vendoring ansible collections and OpenTofu providers so a run does not depend on Galaxy or the tofu registry being reachable |
| [Reacting to alerts](operations/reactive.md) | Wiring a monitor to the hooks, sealing push, writing rules, silencing, answering approvals, asking why a rule did not fire, running check probes and netgroups |
| [Changelog](operations/CHANGELOG.md) | Software releases, features, and fixes — the newest-first release narrative |

## Bill of materials

What ships and under what license — [sbom/index.md](sbom/index.md)

| Page | Covers |
|---|---|
| [Bill of materials](sbom/bill-of-materials.md) | Five layers with pins and licenses, the enforced budget rollup, and the GPL subprocess-boundary rule |

## Contributing

How work lands here and what gates it — [contribution/index.md](contribution/index.md)

## History

Provenance records, imported byte-identical — [history/index.md](history/index.md)

Three candidate designs, the 30-gap critique, eight research notes, and a post-decision
addendum. All but the addendum predate the rename and use the working name "control"; two
of them describe architectures that were not built. Nothing there is normative.

## Conventions

- Every cross-reference in this bundle is a relative link. No absolute paths, no local file
  URLs.
- ADRs are appended and superseded, never rewritten to change meaning.
- Docs move with the change: this bundle is updated in the same commit series as the code
  it describes. If docs and code disagree, the code wins and the correction lands in
  [log.md](log.md).
- Structural changes to this bundle — pages added, deprecated, or moved — are recorded in
  [log.md](log.md). It is a knowledge-base evolution log, not a software changelog.
