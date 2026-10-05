# Operations

Getting an install standing up, and getting it back after it falls over.

| Page | Covers |
|---|---|
| [Bootstrap](bootstrap.md) | The image + Quadlet deploy, the six-screen idempotent wizard, node-zero self-management, the separately versioned `recovery.sh`, and the adoption runbooks for an existing homelab |
| [Containers](containers.md) | The base image and what it carries, running it as shipped, inheriting to add system packages or Python dependencies, offline and multi-arch builds, and capturing an operator's image for recovery |
| [Running the daemon](serve.md) | `exocomp serve` flags, the signal contract, the single-instance lock, and what the CLI does while a daemon is up |
| [Draining for a planned reboot](drain-mode.md) | `exocomp drain` — cordon a running daemon so it fires no schedules and claims no new jobs while in-flight work finishes, poll "safe to reboot?", and list the jobs a reboot left interrupted |
| [Upgrade and rollback](upgrade.md) | `exocomp upgrade`/`rollback` as a ledger choreography — snapshot, stage, verify, swap, health-check — rolling back to the snapshot and prior binary on any failed step; no sudo |
| [Disaster recovery](disaster-recovery.md) | What is backed up, the three legs, the recovery chain, the runbook for a dead host, the recovery card, failure modes, RPO/RTO, and verification |
| [Cold-standby failover](failover.md) | Taking over a replicated store on a standby host — restic restore, optional Litestream fast-forward, deliberate `serve`, and the cross-host active lease that keeps a resurrected primary from double-activating |
| [Offline caches](offline-caches.md) | Vendoring ansible collections and OpenTofu providers so a run does not depend on Galaxy or the tofu registry being reachable |
| [Reacting to alerts](reactive.md) | Pointing a monitor at the hooks, sealing the push transport, writing rules, silencing, answering approvals, and asking why a rule did not fire |
| [Gating tofu plans](plan-policy.md) | Writing CEL policies over a plan's `show -json`, denying a dangerous plan at seal so its apply refuses, and routing an apply through the approval gate |
| [Playbook run flags and config-drift](playbook-run.md) | `playbook run`'s `--limit`/`--tags`/`--skip-tags`/`-e`/`--check`/`--diff`, where extra-var values live (off ansible's argv), and `--check` as a config-drift outcome plus the `--kind play --check` drift schedule |
| [Ad-hoc runs and the unreachable-tolerant outcome](adhoc-and-partial.md) | `command` and `ansible` — the everyday ad-hoc verbs (host pattern, `--shell`, `--become`, `--require-approval`) under the same governance as a play; and `--tolerate-unreachable` on a run or a `--kind play` schedule so an offline roaming host settles the fire degraded instead of paging |
| [Run lifecycle: hooks and the gate](run-lifecycle.md) | A source's `.exocomp/hooks.yml` — ordered pre/post-run steps and a declared-probe health gate — composed into one ledger workflow, honored by `run`, `playbook run --source`, and `handoff run --source` |
| [Terraform→Ansible handoff](terraform-ansible-handoff.md) | Chaining a `tofu apply` to an ansible play as one ledger workflow: the workspace's declared, non-sensitive outputs become the play's inventory, with no tfstate or bearer reaching the play |
| [Ansible-first bootstrap](ansible-first-bootstrap.md) | `exocomp bootstrap run` — the handoff mirrored: the ansible play first, then `tofu apply`, as one ledger workflow. Phase 0 runs the two in order with a manual credential handoff between them |
| [Sealed tofu outputs as run-secrets](sealed-tofu-outputs.md) | A `sensitive = true` output sealed into the envelope and bound to an env name, so the play targeting the workspace's `tf:<workspace>` inventory receives it as `$DB_PASSWORD` with nothing copied by hand |
| [Provision → onboard → converge](provision-converge.md) | `exocomp provision run` as one resumable four-step ledger chain — apply, bounded wait for SSH, seeded onboarding, configure play — and the standing `tf:<workspace>` inventory a `schedule --kind play` targets by name |
| [Sources and credentials](sources.md) | Registering git/host/volume sources, inventories, and the run-secret credential binding; the fleet key, host onboarding, and known_hosts |
| [LAN access](lan-access.md) | Minting bearer tokens and serving the TLS web + phone-approval surface, separate from the loopback state backend ([ADR 0009](../adr/0009-lan-access-tokens.md)) |
| [Device pairing (QR)](pairing.md) | The phone-companion pairing payload a QR carries — url, token, fingerprint, scope, name — rendered on the token-result page and by `token new`, plus setting the public URL |
| [Driving a remote daemon](remote-access.md) | `exocomp login` and `--remote`: TOFU certificate pinning, `--remote`/`EXOCOMP_REMOTE`/`--token` precedence, pinned reads, writes that enqueue on the other box, following a job live, and the inputs that do not cross the wire |
| [Setup and notifications](notifications.md) | The first-run wizard, the ntfy push transport, the MUTED-vs-PAUSED holds, and the `/metrics` staleness gauges |
| [The API surface](api.md) | The `/api/v1` read and write routes on the ADR-0009 listener, the wire envelope, idempotency, and the error-code-to-HTTP-status table |
| [Machine-readable output](machine-output.md) | `--output json`/`EXOCOMP_OUTPUT`, the envelope shape, the error taxonomy, the exit-code table and what of it actually reaches the process today, and MCP-over-HTTP as the third face |
| [The MCP surface](mcp.md) | `POST /mcp` on the ADR-0009 listener, the one agent MCP surface: connecting an agent, what each scope is advertised, why there is no session id, and what never travels |
| [Runner definitions](runners.md) | `exocomp runner add/list/show/rm/import` — a database-backed registry for the deferred container tier; definitions only, nothing executes them yet |
| [Governed CLI runs](cli-run.md) | `exocomp cli-tool` and `exocomp run-cli` — the run type for work that is neither ansible nor tofu: an allow-listed binary under the same queue, sandbox, run-secret and approval governance, with argv that is never a shell string |
| [Changelog](CHANGELOG.md) | The software release narrative — features, fixes, and behavior changes, newest first (kept separate from the knowledge-base evolution log) |

The two are one loop, not two topics: bootstrap does not finish until a restore drill has
passed, so the recovery path is proven before any real work exists to lose.

Underlying decisions: [ADR 0003](../adr/0003-secrets-envelope.md) (the envelope and the
recovery kit) and [ADR 0006](../adr/0006-uv-toolchain-escape-hatch.md) (why DR-path
runbooks always execute the image pins, never the volume toolchain).
