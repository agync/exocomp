# Prior art: homelab control-plane projects

Research date: 2026-08-21. Goal: find an existing project (or small combination of projects) that already
covers most of what a homelab "control stack" needs — running Ansible/Terraform, storing secrets, backing
up its own state, and reacting to alerts — so that we compose instead of building from scratch.

**Target shape, restated:** single-container/single-binary, one operator + a handful of hosts, Ansible-Tower-class
run/inventory/schedule/history + Terraform-Cloud-class plan/apply/state/drift, vault-grade secrets, encrypted
backup to NAS/S3/GitHub, alert-triggered runbooks, TUI-first UI.

No project in this survey covers the full brief. The honest conclusion up front (expanded in
[Recommendation](#recommendation)): **Semaphore UI is the closest single core** for the Ansible+Terraform
run/inventory/schedule/history half, but its Terraform *state backend* and several other goal-relevant
features are Pro-tier (paid), not in the open-source build. Everything else — secrets depth, backup-to-NAS/GitHub,
event-driven triggering, and TUI — has to be composed from adjacent single-purpose tools. Nothing here should
be reinvented; the gaps are glue, not missing primitives.

---

## Landscape table

| Project | Category | Language / runtime | License | Footprint | Single binary/container? | Health (Aug 2026) |
|---|---|---|---|---|---|---|
| [Semaphore UI](https://semaphoreui.com/) | Ansible+Terraform runner (AWX-lite) | Go | MIT (core), **Pro tier paid** for some features | One static Go binary + SQLite/MySQL/Postgres; ~50–100MB RAM idle | Yes (binary or 1 container) | Active — 14k★, commits same-day, [v2.17 released with schedules/syslog](https://semaphoreui.com/releases/semaphore-v2_17) |
| [AWX](https://github.com/ansible/awx) | Ansible runner (Tower upstream) | Python/Django/K8s-operator | Apache-2.0 | Heavy: needs ≥4 vCPU/8GB RAM, Postgres, Redis, ideally K8s | No | Active but enterprise-weight; widely reported as [too heavy for a homelab](https://kestra.io/resources/infrastructure/awx-alternatives) |
| [Rundeck (Community)](https://github.com/rundeck/rundeck) | Job scheduler / runbook automation | Groovy/JVM | Apache-2.0 | JVM + DB (Postgres/H2); heavier than Go tools | No | Maintained by PagerDuty as "Process Automation OSS"; community edition increasingly the neglected tier vs. commercial bundle |
| [Atlantis](https://github.com/runatlantis/atlantis) | Terraform plan/apply via PR comments | Go | Apache-2.0 | Single Go binary; needs a Git host webhook | Yes | Active, 9.2k★, but is *PR-driven only* — no inventory/scheduling/UI dashboard of its own |
| [Terrakube](https://github.com/terrakube-io/terrakube) | Terraform Cloud clone (state, runs, RBAC) | Java (Spring) + React | Apache-2.0 | Multi-service (API, UI, executor, Postgres, Minio) — most TFC-like, least lightweight | No | Small (~950★), slow-moving, thin community |
| [Digger / OpenTaco](https://github.com/diggerhq/digger) | Terraform runs inside your existing CI | Go | MIT | Runs as a CI job, not a standalone service | N/A (no server component) | Active, 5k★, pivoting toward "OpenTaco" state-backend product |
| [Kestra](https://kestra.io/) | General workflow orchestrator (declarative YAML, 1400+ plugins) | Java/JVM | Apache-2.0 (core), EE paid tier | Standalone mode = 1 container, embedded H2 db; production mode wants Postgres/Kafka/Elasticsearch | Standalone: yes | Very active, 27.9k★, daily commits, fast-growing |
| [Windmill](https://www.windmill.dev/) | Script-to-workflow/internal-tool platform | Rust + multi-lang workers | **AGPLv3** (core) + commercial EE | Multi-container (server, worker(s), LSP, Postgres) — not single-binary | No | Very active, 17.6k★, daily commits |
| [Dagu](https://github.com/dagu-org/dagu) | Local-first DAG/cron workflow engine | Go | GPL-3.0 | ~16MB single static binary, **no DB, no broker**, built-in web UI | Yes | Active, 3.8k★, daily commits; small but exactly the lightweight-glue shape this project wants |
| [StackStorm](https://github.com/StackStorm/st2) | Event-driven "IFTTT for ops" (rules/sensors/workflows) | Python | Apache-2.0 | Multi-service (sensor container, rules engine, RabbitMQ/MongoDB or Postgres, Orquesta) — heavy | No | Maintained (commits within days), but architecturally heavy and Python-2-era design debt; adoption has plateaued |
| [Coolify](https://coolify.io/) | Self-hosted PaaS (git-push app deploy) | PHP/Laravel | Apache-2.0 | Single install script, several containers (app, Postgres, Redis) | No (but one-command installer) | Very active, 60.8k★, daily commits — largest community here |
| [Dokploy](https://dokploy.com/) | Self-hosted PaaS (Coolify alternative) | TypeScript | Apache-2.0 **with proprietary carve-outs** (multi-node, schedules, previews, multi-server are non-OSS) | Docker Compose stack (app, Postgres, Traefik) | No | Very active, 36.7k★, fast-growing; [license terms are contested as not truly OSS](https://github.com/ssddanbrown/Open-Source-Confusion-Cases/blob/main/cases/dokploy.md) |
| [Portainer CE](https://www.portainer.io/) | Docker/K8s fleet UI | Go + Vue | Zlib (CE) | Single container | Yes (server) | Very active, 38.3k★, daily commits |
| [Kamal](https://github.com/basecamp/kamal) | SSH-based zero-downtime container deploy | Ruby | MIT | Ruby gem/CLI, no server component at all | Yes (CLI) | Active, 14.5k★; deploy-only, no inventory/state/secrets/scheduling model |
| [clan-core (clan.lol)](https://github.com/clan-lol/clan-core) | Nix-native homelab fleet manager (declarative machines, sops-nix secrets, one-command deploy) | Python + Nix | MIT | CLI + Nix; per-machine footprint is whatever NixOS needs | Yes (CLI) | Small but active (173★, daily commits); requires full NixOS buy-in |
| [onedr0p/cluster-template](https://github.com/onedr0p/cluster-template) | Homelab Kubernetes+Flux GitOps starter (not a product, a template repo) | YAML/Talos/Flux | MIT | N/A — it's a pattern, deploys a whole K8s cluster | N/A | Popular template (2.8k★), actively updated, huge community of forks ("home-ops") |
| [HashiCorp Nomad](https://github.com/hashicorp/nomad) | Workload orchestrator (containers, VMs, binaries) | Go | **BUSL-1.1** (source-available, not OSI since 2023) | Single Go binary, agent+server in one | Yes | Active, 16.8k★; license change is a real adoption headwind for a project meant to stay free forever |
| [OpenBao](https://openbao.org/) | Secrets/PKI vault (Vault fork) | Go | MPL-2.0, Linux Foundation governed | Single Go binary | Yes | Active — v2.5.0 shipped Feb 2026, IBM-backed contributors, genuinely OSS unlike post-2023 Vault |
| [sops](https://github.com/getsops/sops) + [age](https://github.com/FiloSottile/age) | File-level secret encryption (Git-friendly) | Go | MPL-2.0 (sops), BSD-3 (age) | CLI tools, no server | Yes | Mature, CNCF-adopted, extremely stable |
| [ntfy](https://ntfy.sh/) | Push notification relay (webhook → phone/desktop) | Go | Apache-2.0 / GPLv2 dual | Single Go binary + SQLite | Yes | Very active, 33.6k★ |
| [Healthchecks (self-hosted)](https://github.com/healthchecks/healthchecks) | Dead-man's-switch cron monitoring | Python/Django | BSD-3-Clause | Django app + Postgres/SQLite | No (but light) | Active, 10.3k★ |
| [Netmaker](https://github.com/gravitl/netmaker) | WireGuard mesh network control plane | Go | SSPL-ish (see repo) | Server + agents | No | Active, but out of scope — it's a *networking* control plane, not IaC/secrets/backup. Included only as an architecture reference (single control server + lightweight agents, which is a good pattern to imitate) |

---

## Per-option analysis

### Semaphore UI — closest single core for the Ansible+Terraform half

Semaphore is a Go web UI/API that runs Ansible, Terraform/OpenTofu, Bash, PowerShell and Pulumi tasks against
Git-sourced projects, with a project→template→task model, inventories, schedules (cron), and job history —
functionally the AWX/Tower shape, but a single ~binary with SQLite as the default store
([semaphoreui.com](https://semaphoreui.com/), [releases](https://github.com/semaphoreui/semaphore/releases)).

Goal coverage:
- **Ansible runs/inventory/schedule/history:** yes, natively, this is its core purpose.
- **Terraform plan/apply runs:** yes, task templates run `terraform`/`tofu` directly.
- **Terraform *state* storage/locking (the Terraform-Cloud half):** only in the **Pro** (paid) plan via an
  HTTP backend — [state storage, locking, and version history are gated features](https://semaphoreui.com/docs/user-guide/task-templates/apps/terraform/states),
  not in the free build. On the OSS edition you still need your own state backend (S3-compatible, local, etc.).
- **Secrets:** built-in Key Store holds SSH keys, Git credentials, and **Ansible Vault passwords**
  ([Key Store docs](https://semaphoreui.com/docs/user-guide/key-store)); as of 2.16 it can optionally back
  secrets with an external **HashiCorp Vault** instance instead of its own DB
  ([release notes](https://semaphoreui.com/releases/semaphore-v2_16)) — this is the natural place to point at
  OpenBao instead, since OpenBao is Vault-API-compatible.
- **Backup/DR:** project-level export/import exists via UI and CLI
  ([v2.17 adds CLI project export/import](https://semaphoreui.com/releases/semaphore-v2_17)), but there is no
  built-in "back the whole instance up to S3/NAS/GitHub" primitive — community members have open issues asking
  for exactly this ([#1024](https://github.com/semaphoreui/semaphore/issues/1024),
  [#2227 reports restores not working cleanly](https://github.com/semaphoreui/semaphore/issues/2227)). Because
  the whole app state is one SQLite file (or one Postgres/MySQL DB) plus a Git-cloned working tree, wrapping it
  in `restic`/`rclone` is straightforward but is glue we'd have to write.
- **Event-driven triggers:** Semaphore exposes webhooks to *start* a task, so Alertmanager/ntfy/healthchecks can
  call in — but it has no rules engine, no escalation, no "if X then Y" logic beyond "run this template."
- **UI:** web only. No TUI, no desktop app.

Verdict: best available **execution engine**, weakest on state/secrets-depth/backup/reactive logic without
Pro or glue.

### AWX

The literal upstream of what the project explicitly wants to avoid re-inventing badly: full Tower feature set,
but ≥4 vCPU/8GB RAM and ideally Kubernetes to run well ([Kestra's AWX-alternatives piece](https://kestra.io/resources/infrastructure/awx-alternatives)).
Wrong shape for "homelab scale, single operator." Mentioned only as the baseline Semaphore/Rundeck are lighter
alternatives to.

### Rundeck (Community)

JVM job scheduler/runbook tool, Apache-2.0, maintained by PagerDuty as "Process Automation OSS." Solid
scheduling and ACL model, but no Terraform-specific tooling, no built-in secrets vault beyond basic key storage,
heavier runtime than Go-based options, and community sentiment in 2026 is that PagerDuty invests mainly in the
commercial bundle ([community status discussion](https://www.rundeck.com/features)). Not a strong fit.

### Atlantis / Terrakube / Digger — the Terraform-Cloud-shaped options

None of these three, alone, cover the Ansible half at all — they're Terraform-only:

- **Atlantis**: single Go binary, listens for GitHub/GitLab PR webhooks, runs `plan`/`apply` on comment
  commands. No UI dashboard, no inventory, no state storage of its own (you still point it at S3/etc.). Good
  reactive-to-PR model, wrong reactive model for "alert fires → run runbook."
- **Terrakube**: architecturally the *closest* clone of Terraform Cloud (its own state backend, run queue, RBAC,
  UI) but is a multi-service Java+React+Postgres+Minio stack — the opposite of "single-container homelab scale,"
  and community is small/slow (~950★).
- **Digger / OpenTaco**: runs *inside* your existing CI (GitHub Actions, etc.) rather than as a standalone
  service — no persistent server at all, so it doesn't give you a dashboard/history UI unless you build one on
  top of CI logs.

None is a homelab-fit on its own; Semaphore doing both Ansible+Terraform in one lightweight binary beats
stitching one of these in alongside a separate Ansible runner.

### Kestra

A genuinely strong general-purpose workflow orchestrator: declarative YAML flows, huge plugin catalog (Ansible,
Terraform, Shell, HTTP, webhooks-as-triggers are all first-class plugins), and a real **standalone single-container
mode** with an embedded H2 database that starts with one `docker run`
([get-started docs](https://kestra.io/get-started), [self-hosted orchestration overview](https://kestra.io/resources/infrastructure/self-hosted-workflow-orchestration)).
This makes it a credible *reactive automation engine* — an Alertmanager webhook can be a Kestra trigger that
kicks off a flow that shells out to `ansible-playbook` or `terraform apply`. It doesn't natively model
Ansible-style inventory or Terraform-Cloud-style state/drift, though, so it would sit *next to* Semaphore rather
than replace it, which adds a second JVM-class service to the footprint. Community and release cadence (27.9k★,
commits daily) are excellent — this is the most credible "event-driven runbook engine" building block surveyed
if Dagu's plugin ecosystem proves too thin.

### Windmill

Similar territory to Kestra (script-to-workflow, webhooks, scheduling, approval flows) but the **core is
AGPLv3**, not the more permissive Apache-2.0 of Kestra, and it's inherently multi-container (server, one or more
workers, language-server container, Postgres) — not single-binary
([license discussion](https://github.com/windmill-labs/windmill/discussions/3408)). AGPL is fine for pure
self-use, but it's a heavier, more "internal developer platform"-flavored tool than a homelab needs, and offers
no particular Ansible/Terraform-specific ergonomics over Kestra.

### Dagu — the lightweight glue candidate worth calling out

Purpose-built for exactly the "extremely lightweight, single binary" requirement: one ~16MB static Go binary,
**zero external dependencies** (no Postgres, no broker, no Redis), YAML DAGs, a built-in web UI, and native
step types for shell, SSH, Docker, and Kubernetes
([dagu.sh](https://dagu.sh/), [GitHub](https://github.com/dagu-org/dagu)). It is materially lighter than Kestra
or Windmill and a much better footprint match. Its trade-off is a much smaller plugin/trigger ecosystem — no
1400-plugin catalog, so wiring Alertmanager/ntfy/healthchecks webhooks in means calling Dagu's own
start-workflow API rather than using a pre-built trigger connector. For a homelab-scale "event fires → shell out
to ansible-playbook/terraform" engine, Dagu is arguably the best-shaped single component in this whole survey —
small, active (commits daily), and it does one thing.

### StackStorm

The literal "event-driven automation for auto-remediation" tool the goals describe (rules engine + sensors +
workflow engine + 6000+ pack actions, [github.com/StackStorm/st2](https://github.com/StackStorm/st2)) — but its
architecture (RabbitMQ/MongoDB or Postgres, multiple Python services, Orquesta workflow engine) is exactly the
enterprise-weight shape this project is trying to avoid. It is maintained (commits within days as of this
research) but is not trending toward lighter-weight; treat it as a reference for *what a rules engine should be
able to express* (if X alert then run Y runbook with escalation) rather than something to deploy.

### Coolify / Dokploy / Portainer / Kamal — app-deployment PaaS tools, not IaC control planes

These four are excellent at what they do — git-push container deployment with TLS, multi-server orchestration,
and (for Coolify) scheduled DB backups to S3-compatible storage
([Coolify backup docs](https://massivegrid.com/blog/coolify-backup-strategy/)) — but none of them run Ansible
playbooks or Terraform plans, and none stores infrastructure state. They solve "how do I deploy and keep alive
the *apps* running on my hosts," which is adjacent to but distinct from "how do I provision/configure the hosts
and store secrets/state for that." Coolify's backup-to-S3 pattern and Dokploy's lightweight footprint
(0.8GB idle RAM vs Coolify's 1.2GB, per
[nextgrowth.ai's comparison](https://nextgrowth.ai/coolify-vs-dokploy/)) are worth borrowing patterns from, not
platforms to extend. Dokploy's license is also worth flagging: several features (multi-node, schedules, preview
deployments, multi-server) are carved out under a modified, non-OSI license, which the community has flagged as
[not actually meeting the open-source definition](https://github.com/ssddanbrown/Open-Source-Confusion-Cases/blob/main/cases/dokploy.md) —
a real risk if the project ever depended on those features. Kamal is CLI-only deploy tooling (SSH + Docker +
Traefik), with no server/state/UI at all — closest in spirit is the community `lazykamal` TUI wrapper
([shuvro.io](https://shuvro.io/posts/lazykamal-kamal-tui-hetzner-multi-app-hosting/)), a useful proof that a thin
TUI over a scriptable CLI backend is a viable, low-effort way to satisfy the "TUI first" requirement without
building a TUI framework from scratch.

### Homelab GitOps template repos (onedr0p/cluster-template and the "home-ops" family)

Not a product — a widely-forked template ([2.8k★](https://github.com/onedr0p/cluster-template)) that wires
Talos Linux + Kubernetes + Flux + SOPS + Renovate into a GitOps homelab pattern. It answers "how do I manage
config drift and secrets for my cluster declaratively," largely by committing to Kubernetes as the substrate.
This is a legitimate *alternative architecture* to the control-stack goals (drift management becomes Flux's
job, "runs" become Flux reconciliations, "secrets" become sops-in-git) — but it presupposes the operator wants
to run Kubernetes at all, which the stated goals don't require, and it doesn't address Ansible-style
host-level configuration, Terraform Cloud-style plan/apply workflow, or event-driven runbooks outside the
Kubernetes control loop. Worth citing as the dominant pattern in the homelab GitOps community, not as something
to extend.

### clan.lol / clan-core

A Nix-native fleet manager: declarative per-machine configuration, a `vars`/secrets system built on
**sops-nix** (per-machine age keys, automatic decrypt-to-tmpfs at boot,
[docs.clan.lol/guides/secrets](https://docs.clan.lol/guides/secrets/)), and one-command `clan machines update`
deployment over SSH. It is a genuinely elegant secrets+deploy story — but only if the whole fleet runs NixOS,
which is a much bigger commitment than "compose existing tools around a homelab." Its secrets architecture
(per-machine keys, sops-encrypted vars checked into Git, decrypted only in memory at activation) is a strong
reference design for this project's own secret-backup-to-Git requirement, independent of whether Nix itself is
adopted.

### HashiCorp Nomad

Single-binary workload orchestrator that can schedule containers, raw binaries, and VMs — architecturally very
close to "lightweight orchestrator for a homelab." The blocker is the license: Nomad moved to **BUSL-1.1** in
2023 ([LICENSE](https://github.com/hashicorp/nomad/blob/main/LICENSE)), a source-available-not-OSI license that
converts to MPL only after four years per release. For a project whose own goals emphasize durable,
recoverable, long-lived homelab infrastructure, building on a vendor's source-available license is a risk the
OpenBao/Vault fork precedent shows can materialize (see below). Nomad is a scheduler, not a
secrets/backup/config-runner anyway, so it's out of scope even setting licensing aside.

### Secrets layer: OpenBao vs. sops+age

Two genuinely complementary, not competing, tools:

- **OpenBao**: the Linux-Foundation-governed, MPL-2.0 fork of HashiCorp Vault created after Vault's 2023 BUSL
  relicensing. As of early 2026 it's at v2.5.0 with IBM engineering involvement, and — notably for a homelab —
  **namespaces and horizontal read scalability, which are Enterprise-only in real Vault, are free in OpenBao
  core** ([openlogic.com comparison](https://www.openlogic.com/blog/hashicorp-vault-alternatives-openbao-vs-vault)).
  Single Go binary, API-compatible with Vault, so anything built to speak the Vault API (including Semaphore's
  Vault integration) works unmodified against OpenBao. This is the right answer for *dynamic* secrets
  (short-lived cloud credentials, PKI).
- **sops + age**: not a server at all — encrypts values in-place inside YAML/JSON/ENV files with per-recipient
  age keys, so encrypted secrets live directly in Git next to the manifests they configure
  ([sops repo](https://github.com/getsops/sops), CNCF-adopted). This is the right answer for *static* secrets
  (ansible-vault passwords, terraform variable files, backup-target credentials) that should travel with the
  backup-to-GitHub requirement — an encrypted-to-GitHub backup of "its own state/config/secrets" is essentially
  free if the secrets were already sops-encrypted flat files.

Recommendation: use both, not one — sops/age for anything that should ride along in a Git-based backup, OpenBao
(pointed to by Semaphore's built-in Vault integration) for anything that needs dynamic issuance or centralized
audit.

### Backup primitives

No single project in this survey does "encrypted backup of my own state to NAS(SMB/NFS)/S3/GitHub" as a
first-class feature — it is glue everywhere (Semaphore's open backup-feature-request issues confirm this gap
directly). The building blocks are mature and boring, which is a good thing:
- **restic** — content-addressed, encrypted, dedup backups with native S3-compatible and SFTP/rest-server
  backends; add an SMB/NFS mount and it's a local target too.
- **rclone** — the general-purpose "copy encrypted archive to (S3-compatible | SMB | NFS | a Git-hosted release
  asset)" mover; not itself an encryption tool, so pair with sops/age or restic for the encryption step.
- A GitHub-repo backup target is just "encrypt with age, `git commit`, `git push`" — no special tool needed
  once secrets are already sops/age-encrypted flat files, which loops back to the clan.lol-style secrets
  pattern above.

### Event-driven trigger glue

- **ntfy** — single Go binary, push-notification relay, dual Apache-2.0/GPLv2, 33.6k★, trivially receives a
  webhook and fans out to phone/desktop ([ntfy.sh](https://ntfy.sh/)).
- **healthchecks (self-hosted)** — dead-man's-switch cron monitoring; Django app + Postgres/SQLite, BSD-3,
  10.3k★; can itself POST to Alertmanager or ntfy on failure
  ([self-hosted docs](https://healthchecks.io/docs/self_hosted/)).
- **Alertmanager** — already the de facto standard if Prometheus is in the stack; its generic webhook receiver
  is the natural "alert fired → call my control plane's webhook" bridge
  ([pistack.xyz alert-routing comparison](https://www.pistack.xyz/posts/2026-05-07-prometheus-alertmanager-vs-ntfy-vs-gotify-self-hosted-alert-routing-guide/)).

None of these are runbook *engines* — they're the trigger side. The engine that turns "webhook received" into
"run this Ansible/Terraform job" is the gap Semaphore's own webhooks can fill directly for simple cases, or
Dagu/Kestra for anything needing conditional logic, retries, or escalation chains.

---

## Integration notes relative to project goals

| Goal | Best-fit component(s) | Gap that needs glue |
|---|---|---|
| Ansible run/inventory/schedule/history | Semaphore UI | none significant |
| Terraform plan/apply runs | Semaphore UI | none significant |
| Terraform state storage/drift | none free | Semaphore's state backend is Pro-only; use a plain S3-compatible bucket (MinIO/Backblaze) as the Terraform backend directly, outside Semaphore, until/unless Pro is worth paying for |
| ansible-vault integration | Semaphore UI (native) | none |
| Vault-class dynamic secrets | OpenBao, via Semaphore's built-in Vault connector | none — this is a clean seam |
| Git-friendly static secrets | sops + age | none — mature, boring, correct |
| Encrypted backup to NAS/S3/GitHub | restic + rclone (+ sops/age for the Git target) | Semaphore/Kestra/Dagu have no built-in "back myself up" button — this is the one place real glue-scripting is required |
| Reactive/event-driven runbooks | Dagu (lightest) or Kestra (richer plugin/trigger ecosystem) triggered by Alertmanager/ntfy/healthchecks webhooks, which then call Semaphore templates | rules/escalation logic (StackStorm-class) is genuinely absent from the lightweight options; likely acceptable to hand-roll simple if/then in the chosen workflow engine rather than adopt StackStorm |
| TUI-first UI | none of the surveyed projects — Semaphore, Kestra, Dagu, Coolify, Dokploy are all web-first | build a thin TUI (à la the community `lazykamal` pattern) against Semaphore's REST API + the workflow engine's API; this is unavoidable custom work but is genuinely thin glue, not a control plane |
| Trivial bootstrap/DR | Semaphore's single-binary/SQLite start is the easiest of the run-engine options; DR is only as good as the backup glue above | document + script a restore path explicitly — no project here provides it out of the box |

---

## Recommendation

**Do not build a from-scratch control plane. Compose around Semaphore UI as the execution core**, and treat
everything else as well-understood, individually mature building blocks wired together with a thin layer of
scripts/TUI — which matches the stated preference ("compose existing open-source tools, build thin glue only
where needed").

Concretely:

1. **Run engine — Semaphore UI (OSS/MIT).** Covers Ansible playbooks, Terraform/OpenTofu plans/applies,
   inventories, cron schedules, and job history in one Go binary. Accept that Terraform *state* storage is not
   included free — point Terraform at a plain S3-compatible bucket (self-hosted MinIO or a cheap external
   provider) as the backend instead of paying for or reimplementing Semaphore's Pro HTTP backend.
2. **Secrets — sops + age for static secrets committed alongside config; OpenBao for anything dynamic**,
   reached through Semaphore's existing Vault-compatible connector so no custom secret-fetching code is needed
   inside Semaphore itself.
3. **Backup/DR — restic (or rclone) wrapped in a small script/cron job** that snapshots Semaphore's SQLite DB +
   its Git-cloned project trees + the sops/age-encrypted secrets to NAS (SMB/NFS mount), an S3-compatible
   bucket, and/or a private GitHub repo. This is the one place with no existing project to lean on — budget it
   as the actual "thin glue" deliverable.
4. **Event-driven runbooks — Dagu as the trigger/orchestration layer**, called by Alertmanager/healthchecks/ntfy
   webhooks, which in turn calls Semaphore's task-template webhook to actually execute the Ansible/Terraform
   job. Reach for Kestra instead of Dagu only if the trigger/plugin catalog Dagu lacks (native Slack/PagerDuty
   connectors, complex conditional flows) turns out to matter in practice — it costs a second JVM-class service
   to get there.
5. **UI — TUI as a thin client over Semaphore's REST API** (and Dagu's, if added), following the proof-of-concept
   pattern the community already validated with `lazykamal` for Kamal. Web UI is already provided free by
   Semaphore/Dagu for the "then web UI" fallback tier; no desktop app is needed at homelab scale.

What we explicitly rejected and why:
- **AWX / StackStorm / Terrakube / Windmill** — right feature set, wrong weight class (multi-service, JVM/Python
  stacks, Postgres+broker dependencies) for "single operator, a handful of hosts."
- **Nomad** — right footprint, wrong license (BUSL) and wrong problem (workload scheduling, not
  config/state/secrets management).
- **Coolify / Dokploy / Portainer / Kamal** — excellent at deploying *applications*, but none of them run
  Ansible/Terraform or store infrastructure state; adjacent problem, not this one. Dokploy's license also has
  open community disputes about whether the gated features are really open source.
- **clan.lol / onedr0p-style K8s GitOps** — both are complete, opinionated alternate architectures (full Nix
  buy-in, or full Kubernetes buy-in) rather than composable components; worth mining for design patterns
  (per-machine sops-nix secrets; Flux-style reconciliation) but not worth adopting wholesale unless the project
  is willing to commit to Nix or Kubernetes as the substrate, which the stated goals don't ask for.
