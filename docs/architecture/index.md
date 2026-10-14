# Architecture

How exocomp is put together, and which parts of "works with" are contractual.

| Page | Covers |
|---|---|
| [Overview](overview.md) | Component map, the one-queue convergence property, state layout, and the standing commitments |
| [Platform contract](platform-contract.md) | Support tiers for systemd/Quadlet, rootless podman, docker-compose, and SELinux enforcing; the specified remote-access path |
| [Execution](execution.md) | The `ExecutorDriver` tiers `disabled`, `enforcing`, and `container`; offline-restorable job dependencies; boot-time reconciliation |
| [Reactive layer](reactive.md) | Webhook ingress, the one event shape, CEL rules, the guardrails in ledger order, approvals, and drift as an event |

Decisions behind these pages: [Decision 0001](../decisions/0001-thin-core-architecture.md)
(thin-core), [Decision 0004](../decisions/0004-opentofu-http-state-backend.md) (state backend),
[Decision 0005](../decisions/0005-job-sandboxing-nono.md) (sandboxing).
