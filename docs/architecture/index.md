# Architecture

How exocomp is put together, and which parts of "works with" are contractual.

| Page | Covers |
|---|---|
| [Overview](overview.md) | Component map, the one-queue convergence property, state layout, and the standing commitments |
| [Platform contract](platform-contract.md) | Support tiers for systemd/Quadlet, rootless podman, docker-compose, and SELinux enforcing; the specified remote-access path |
| [Execution](execution.md) | The `ExecutorDriver` tiers `local`, `local-sandboxed`, and `container`; offline-restorable job dependencies; boot-time reconciliation |

Decisions behind these pages: [ADR 0001](../adr/0001-thin-core-architecture.md)
(thin-core), [ADR 0004](../adr/0004-opentofu-http-state-backend.md) (state backend),
[ADR 0005](../adr/0005-job-sandboxing-nono.md) (sandboxing).
