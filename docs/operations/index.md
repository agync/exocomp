# Operations

Getting an install standing up, and getting it back after it falls over.

| Page | Covers |
|---|---|
| [Bootstrap](bootstrap.md) | `install.sh`, the six-screen idempotent wizard, node-zero self-management, and the adoption runbooks for an existing homelab |
| [Running the daemon](serve.md) | `exocomp serve` flags, the signal contract, the single-instance lock, and what the CLI does while a daemon is up |
| [Disaster recovery](disaster-recovery.md) | What is backed up, the three legs, the recovery chain, the runbook for a dead host, the recovery card, failure modes, RPO/RTO, and verification |
| [Offline caches](offline-caches.md) | Vendoring ansible collections and OpenTofu providers so a run does not depend on Galaxy or the tofu registry being reachable |
| [Reacting to alerts](reactive.md) | Pointing a monitor at the hooks, sealing the push transport, writing rules, silencing, answering approvals, and asking why a rule did not fire |
| [Gating tofu plans](plan-policy.md) | Writing CEL policies over a plan's `show -json`, denying a dangerous plan at seal so its apply refuses, and routing an apply through the approval gate |
| [Sources and credentials](sources.md) | Registering git/host/volume sources, inventories, and the run-secret credential binding; the fleet key, host onboarding, and known_hosts |
| [LAN access](lan-access.md) | Minting bearer tokens and serving the TLS web + phone-approval surface, separate from the loopback state backend ([ADR 0009](../adr/0009-lan-access-tokens.md)) |
| [Setup and notifications](notifications.md) | The first-run wizard, the ntfy push transport, the MUTED-vs-PAUSED holds, and the `/metrics` staleness gauges |

The two are one loop, not two topics: bootstrap does not finish until a restore drill has
passed, so the recovery path is proven before any real work exists to lose.

Underlying decisions: [ADR 0003](../adr/0003-secrets-envelope.md) (the envelope and the
recovery kit) and [ADR 0006](../adr/0006-uv-toolchain-escape-hatch.md) (why DR-path
runbooks always execute the image pins, never the volume toolchain).
