# Operations

Getting an install standing up, and getting it back after it falls over.

| Page | Covers |
|---|---|
| [Bootstrap](bootstrap.md) | `install.sh`, the six-screen idempotent wizard, node-zero self-management, and the adoption runbooks for an existing homelab |
| [Disaster recovery](disaster-recovery.md) | What is backed up, the three legs, the recovery chain, the runbook for a dead host, the recovery card, failure modes, RPO/RTO, and verification |

The two are one loop, not two topics: bootstrap does not finish until a restore drill has
passed, so the recovery path is proven before any real work exists to lose.

Underlying decisions: [ADR 0003](../adr/0003-secrets-envelope.md) (the envelope and the
recovery kit) and [ADR 0006](../adr/0006-uv-toolchain-escape-hatch.md) (why DR-path
runbooks always execute the image pins, never the volume toolchain).
