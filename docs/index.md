---
okf_version: "0.2"
---

# exocomp knowledge base

**exocomp** is a homelab control plane: one Go binary (`exocompd`) — running as a systemd
host service or rootless container — that owns state, sequencing, keys, and recovery,
shelling out to pinned tools (`ansible-core`, `opentofu`, `restic`, `sops`, `age`, `git`) for
everything else.

Scale is one operator and a handful of hosts. Two constraints rule every decision here:
**trivial bootstrap** and **trivial disaster recovery**.

---

## 1. User guide (front and center)

Task-oriented workflows and step-by-step guides for everyday homelab management — [guide/index.md](guide/index.md).

| Guide | Covers |
|---|---|
| [Quickstart](guide/quickstart.md) | 15-minute quickstart: Quadlet deployment, first-run wizard, and printing the recovery card |
| [Sources and inventory](guide/sources.md) | Registering Git repositories, local directories, SSH fleet keys, and auto-onboarding hosts |
| [Ansible automation](guide/ansible.md) | Running playbooks, ad-hoc execution, `--privileged` API egress, and `--check` config drift |
| [OpenTofu infrastructure](guide/opentofu.md) | Workspaces, native SQLite HTTP state backend, plan diffs, CEL policies, and sealed outputs |
| [Provision-to-converge handoffs](guide/handoffs.md) | Chaining OpenTofu apply to Ansible playbook execution in a single atomic ledger workflow |
| [Secrets and fleet keys](guide/secrets.md) | Age envelope encryption, SOPS integration, run-secret bindings, and zero secret leakage |
| [Job sandboxing](guide/sandbox.md) | nono tiers, default-deny grants, egress scoping, the `--privileged` opt-out, and operational limits |
| [Health checks and probes](guide/checks.md) | Reachability matrix, CIDR network groups, probe targets, and check-triggered runs |
| [Reactive automation](guide/reactive.md) | Webhooks, CEL event rule routing, push alerts, silence windows, and phone approvals |
| [Mobile companion](guide/mobile.md) *(in development)* | Pairing phone via QR code, ntfy push notifications, and one-thumb approval decisions |
| [AI agent integration](guide/agent-mcp.md) | Model Context Protocol (`POST /mcp`) setup with scoped bearer tokens for Claude Code / Cursor |

---

## 2. Design and visual showcase (front and center)

Interface design as a functional contract — [design/index.md](design/index.md).

| Page | Covers |
|---|---|
| [Visual showcase & gallery](design/gallery.md) | **Interface tour**: High-resolution screenshots of Web Dashboard (dark & light), OpenTofu plan diffs, live log tails, checks matrix, and TUI |
| [Design invariants](design/INVARIANTS.md) | The five immutable design principles, color-plus-word rules, and UI vocabulary |
| [Web design system](design/web-system.md) | CSS tokens, typography scale, tabular number invariants, and WCAG AA contrast rules |
| [Terminal UI (TUI)](design/tui-system.md) | Bubble Tea/Lip Gloss terminal surface, 80x24 layout, keymaps, and Row 24 powerline strip |
| [Machine token specification](design/DESIGN.md) | Full token catalog transcribed from `internal/web/tokens.css` for automated agents |

---

## 3. Contributor guide (front and center)

How work lands here and what gates it — [contribution/index.md](contribution/index.md).

| Guide | Covers |
|---|---|
| [Developer setup](contribution/dev-setup.md) | Toolchain prerequisites (Go 1.23+, rootless Podman, `nono` sandbox) and git hooks activation |
| [Developer workflow](contribution/dev-workflow.md) | Running `exocompd` in development mode, `templ` template generation, and local CLI testing |
| [Testing and verification gates](contribution/testing.md) | `make check` fast loop, `make check-race`, mutation testing of safety guards, and Lima VM testbeds |
| [Codebase architecture map](contribution/architecture.md) | Package relationships, unidirectional store boundaries, the step ledger, and the nono execution driver |
| [Worktree lanes & branch hygiene](contribution/worktrees.md) | Parallel work in `.worktrees/`, linear commit history on `main`, and commit message conventions |
| [Schema migrations](contribution/migrations.md) | Authoring forward-only SQLite migrations, `PRAGMA user_version`, and collision resolution |

---

## 4. Operations and cluster lifecycle

Day-2 production operations, node-zero deployment, disaster recovery drills, and cluster maintenance — [operations/index.md](operations/index.md).

| Runbook | Covers |
|---|---|
| [Bootstrap and deployment](operations/bootstrap.md) | Container Quadlet deploy, node-zero self-management, and adoption runbooks |
| [Running the daemon](operations/serve.md) | `exocomp serve` flags, process lifecycle, signal handling, and single-instance locks |
| [Planned reboot & drain mode](operations/drain-mode.md) | `exocomp drain` — cordoning the daemon, draining in-flight jobs, and reboot safety |
| [Upgrade and rollback](operations/upgrade.md) | Zero-sudo upgrade choreography: mandatory pre-upgrade snapshots, binary swap, rollback |
| [Containers and derived images](operations/containers.md) | Shipped OCI image, offline builds, and derived `FROM exocomp` custom containers |
| [Disaster recovery](operations/disaster-recovery.md) | The 3 backup legs (NAS/S3/Git), recovery kit, and restoring onto a dead host |
| [Cold-standby failover](operations/failover.md) | Replicated store takeover, Litestream fast-forward, and split-brain active lease guards |
| [Offline caches and vendoring](operations/offline-caches.md) | Vendoring Ansible collections and OpenTofu providers for airgapped operations |

---

## 5. Technical reference

Precise specifications and wire protocols — [reference/index.md](reference/index.md).

| Reference | Covers |
|---|---|
| [Command line interface (CLI)](reference/cli.md) | Syntax, commands, global flags, and JSON envelope output (`--output json`) |
| [Configuration files](reference/configuration.md) | Formats and schemas for `.exocomp/hooks.yml`, Quadlet unit definitions, and policies |
| [Exit codes and error taxonomy](reference/exit-codes.md) | Exit code mappings, error domains, and failure classifications |
| [REST API (`/api/v1`)](operations/api.md) | Wire endpoints, authentication, idempotency headers, and HTTP status mappings |
| [MCP tool catalog](operations/mcp.md) | Model Context Protocol tools, input schemas, and scope definitions |

---

## 6. Architecture & decision records

Foundational architectural design and immutable decisions — [architecture/index.md](architecture/index.md) & [decisions/index.md](decisions/index.md).

- [Architecture overview](architecture/overview.md): Component map, the one-queue convergence property, state layout.
- [Execution driver](architecture/execution.md): Sandbox tiers (`disabled`, `enforcing` with `nono`, `container`).
- [Platform contract](architecture/platform-contract.md): Support tiers for systemd, Podman, and SELinux enforcing.
- [Reactive layer](architecture/reactive.md): CEL evaluation, guardrails, and ledger ordering.
- [Decision records 0001–0022](decisions/index.md): Foundational architecture decision records.

---

## 7. Security & SBOM

- [Threat model](security/threat-model.md): Assets, threat actors, attack surfaces, and accepted residuals.
- [Bill of materials](sbom/bill-of-materials.md): Component pins, licenses, and memory/binary size budgets.

---

## Conventions

- Every cross-reference in this bundle is a relative link.
- Decision records are appended and superseded, never rewritten to change meaning.
- Docs move with code: structural changes are recorded in [log.md](log.md).
