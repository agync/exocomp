# Self-hosted Ansible execution/orchestration platforms for a homelab control stack

**Scope:** Compare self-hostable options for running Ansible (and, where relevant, Terraform/OpenTofu)
playbooks with inventory, scheduling, job history, an API, and event-driven triggers — sized for a
single-operator homelab (a handful of hosts), not an enterprise fleet. Evaluated: AWX, Semaphore UI,
Rundeck, ansible-runner (as a library), ansible-navigator, and plain systemd timers + `ansible-pull`.

Researched August 2026. Every claim below is sourced inline; treat pricing/feature-tier details as
subject to change and re-verify against the vendor docs before committing to a design.

---

## Landscape table

| Name | Language / runtime | License | Footprint | Single-binary/container? | Maintenance health (2025–2026) |
|---|---|---|---|---|---|
| **AWX** | Python/Django + React, Postgres, Redis, runs on Kubernetes (via awx-operator) | Apache 2.0 | Heavy: recommended prod sizing is 4 CPU / 16 GB RAM / 100 GB SSD for Postgres alone; even minikube dev guidance wants 4 CPU / 8 GB RAM ([AWX Operator sizing](https://ansible-awx.mintlify.app/installation/requirements), [forum thread](https://forum.ansible.com/t/what-are-the-hardware-requirements-for-deploying-awx-in-kubernetes/43526)) | No — multi-pod Kubernetes app (web, task, Postgres, Redis, Postgres) | **Stalled.** Last stable release 24.6.1, July 2, 2024. Releases paused mid-2024 for a "large-scale refactoring" to a pluggable architecture; as of the ongoing community thread, no release timeline exists and Red Hat acknowledged "the crickets aren't great" ([Is there a future for AWX? — Ansible Forum](https://forum.ansible.com/t/is-there-a-future-for-awx/44527)). `devel` branch still sees commits but nothing shippable. |
| **Semaphore UI** | Go backend + Vue frontend, single static binary, BoltDB/Postgres/MySQL | MIT (core); Pro/Enterprise tiers are commercial add-ons | Light: single Go binary or single Docker container; BoltDB needs no external DB ([GitHub README](https://github.com/semaphoreui/semaphore), [Docker image](https://hub.docker.com/r/semaphoreui/semaphore)) | **Yes** — single binary or single container is the primary distribution model, plus Deb/RPM/Snap packages and marketplace VM images | **Active.** v2.16 shipped Aug 6, 2025 with patch releases into Jan 2026; v2.17 shipped Feb 15, 2026 (one-time schedules, syslog, project export/import CLI) ([v2.17 release notes](https://semaphoreui.com/releases/semaphore-v2_17)). ~14k GitHub stars, 5,200+ commits on `develop`, 200+ open PRs — healthy velocity. |
| **Rundeck** | Java/Grails (JVM), needs a JVM heap + optional Postgres/MySQL | Apache 2.0 (Community); Enterprise edition is commercial, owned by PagerDuty | Medium-heavy: JVM wants ~2 CPU / 4 GB RAM minimum for production; combined Rundeck+Postgres container guidance is ~2 GB RAM floor ([Docker config reference](https://docs.rundeck.com/docs/administration/configuration/docker.html)) | Runs as one WAR/container, but it's a full JVM app server, not a lightweight binary | **Active but enterprise-steered.** Apache 2.0, GitHub shows commits/issues through Aug 2026, ~60k community users claimed ([Rundeck open source](https://www.rundeck.com/open-source)). Owned by PagerDuty since the Rundeck Inc. acquisition; Community edition is intentionally feature-gated relative to Enterprise (e.g., webhook "Advanced Run Job" handler is Enterprise-only, Community gets basic "Run Job" — [Rundeck webhooks docs](https://docs.rundeck.com/docs/learning/howto/using-webhooks.html)). |
| **ansible-runner** | Python library / CLI, no server component | Apache 2.0 | Negligible — it's a library, not a service; you provide the wrapper (cron, systemd, a custom API, etc.) | N/A — not a platform, it's a building block AWX/Semaphore-style tools are built on | **Active.** 2.4.3 released Mar 16, 2026, 2.4.2 Oct 14, 2025, 2.4.1 Mar 2025 — steady cadence ([PyPI](https://pypi.org/project/ansible-runner/), [GitHub releases](https://github.com/ansible/ansible-runner/releases)). |
| **ansible-navigator** | Python, terminal UI over ansible-runner + optional container "execution environments" | Apache 2.0 | Light — a CLI/TUI, not a server; footprint is whatever execution-environment image you point it at | N/A — CLI tool, not a persistent service (no job history, scheduling, or API of its own) | **Active**, maintained under the Ansible org; docs and deep-dive guides current through mid-2026 ([GitHub](https://github.com/ansible/ansible-navigator)). |
| **systemd timers + `ansible-pull`** | Bash/systemd + Ansible itself | GPLv3+ (Ansible) | Minimal — no server at all; each managed host pulls its own config | Effectively single-binary in the sense that there's no orchestrator process to run anywhere | Ansible core itself is actively maintained; the pattern is a DIY convention, not a product, so "health" is really "how much glue you're willing to own." |

---

## Per-option analysis

### AWX
AWX is the open-source upstream of Red Hat Ansible Automation Platform: Django/React web UI, REST API,
Postgres for state, Redis for the task queue, and a task-execution layer, deployed via the awx-operator
onto Kubernetes (k3s/minikube included). It has the deepest feature set of anything here — full RBAC,
credential types, surveys, workflow visualizer, notifications, webhooks from GitHub/GitLab — because it's
designed to be Ansible Tower.

That depth is also the problem for a homelab. Recommended production sizing is 4 CPU / 16 GB RAM / 100 GB
SSD ([AWX docs](https://ansible-awx.mintlify.app/installation/requirements)); even a throwaway minikube
dev instance wants 4 CPU / 8 GB RAM. It has no supported single-container or single-binary path — you are
running Kubernetes (or at minimum a multi-container Docker Compose stack maintained outside the official
project) to get a web pod, task pod, Postgres, and Redis all talking to each other.

More importantly for a 2025-2026 decision: **the project has been effectively frozen since July 2024.**
The last stable release is 24.6.1. Development paused for a "pluggable, service-oriented architecture"
refactor with no announced completion date; a community thread titled "Is there a future for AWX?" is
still open with Red Hat staff acknowledging poor communication
([forum thread](https://forum.ansible.com/t/is-there-a-future-for-awx/44527)). Vault handling and
webhooks are both solid *when it works* ([webhooks docs](https://docs.ansible.com/projects/awx/en/24.6.1/userguide/webhooks.html)),
but adopting AWX today means betting a homelab's control plane on a project in an indefinite hiatus.

### Semaphore UI (semaphoreui.com)
Semaphore is a Go application (Vue frontend) that ships as a genuinely single binary or single container,
with BoltDB as an embedded zero-dependency default database (Postgres/MySQL optional for larger setups).
It runs Ansible playbooks, and — confirmed current as of v2.17 (Feb 2026) — also runs **Terraform, OpenTofu,
Terragrunt, Pulumi, Bash, and PowerShell** as first-class task types, with variable groups, RBAC, scheduling,
a REST API, and an inbound webhook/integration system where external events (GitHub pushes, monitoring
alerts, chat commands) can trigger a task template via a per-integration URL with matchers/extractors
([Integrations docs](https://semaphoreui.com/docs/user-guide/integrations)). Outbound webhooks (START/
SUCCESS/FAILURE events, HMAC-signed) are also supported. Ansible Vault passwords are handled via the
built-in Key Store, and secrets are encrypted at rest in the database using an operator-supplied key
(`SEMAPHORE_ACCESS_KEY_ENCRYPTION`) ([Key Store docs](https://semaphoreui.com/docs/user-guide/key-store)).
Deployment genuinely takes minutes with a single `docker run` or the prebuilt binary
([alphabravo.io writeup](https://blog.alphabravo.io/the-ultimate-command-center-managing-terraform-and-ansible-with-semaphore-ui/),
[virtualizationhowto.com](https://www.virtualizationhowto.com/2025/07/ansible-semaphore-ui-my-new-go-to-infrastructure-automation-tool/)).

Two things to know before committing:

1. **Licensing tiers matter for the Terraform-Cloud-replacement use case.** Core Semaphore is MIT and
   free forever. But the Terraform/OpenTofu HTTP state backend (state storage + locking + version
   history inside Semaphore) and HashiCorp Vault secret-backend integration are **Pro-tier** features
   ($490/yr as of writing, up to 500 nodes), and HA/LDAP/SSO/custom-roles are Enterprise-tier
   ([Pro pricing page](https://semaphoreui.com/pro), [feature-tier summary](https://semaphoreui.com/blog/awx-vs-semaphore)).
   The free tier covers running Terraform, but *not* Semaphore-hosted remote state — you'd point
   Terraform at your own S3-compatible backend/state locking instead, which is standard practice anyway
   and fits this project's "backup to S3-compatible providers" goal.
2. Release cadence is healthy and recent: v2.16 (Aug 2025, with HashiCorp Vault integration and
   Swagger docs — [release notes](https://semaphoreui.com/releases/semaphore-v2_16)) through v2.17
   (Feb 2026, one-time schedules, syslog, CLI project export/import —
   [release notes](https://semaphoreui.com/releases/semaphore-v2_17)), with patch releases through
   January 2026. ~14,000 GitHub stars, active issue/PR throughput.

### Rundeck
Rundeck is the oldest tool here (2010) and the most general — it's a job-runner/self-service-ops platform
that happens to support Ansible as one of many "node executor" plugins, alongside SSH, scripts, and
others; it's not Ansible-native the way AWX or Semaphore are. It's a JVM app (Grails), so it wants real
memory: ~2 GB RAM minimum for a combined Rundeck+Postgres container, more (4 GB+) if the database is
separate and under load ([Docker config reference](https://docs.rundeck.com/docs/administration/configuration/docker.html)).
It runs as a single container/WAR, but "single container" here still means a JVM with meaningful idle
overhead, not a lightweight static binary.

Licensing: Apache 2.0 core, actively committed through August 2026, but now owned by PagerDuty and
positioned as "PagerDuty Process Automation" commercially. The webhook system is functional but
deliberately tiered — Community edition gets the basic "Run Job" webhook handler, while "Advanced Run
Job" (richer payload handling) is Enterprise-only
([Rundeck webhooks docs](https://docs.rundeck.com/docs/learning/howto/using-webhooks.html)). Ansible-vault
handling isn't a first-class concept in Rundeck the way it is in the Ansible-native tools; you'd manage
it through Rundeck's own key storage plus an Ansible node-executor wrapper. For a project whose core
requirement is "Ansible + Terraform orchestration," Rundeck brings generality this use case doesn't need,
at a heavier resource cost than Semaphore, without a compelling offsetting advantage.

### ansible-runner (as a library)
Not a platform — a Python library/CLI (and the execution engine AWX itself uses under the hood) for
invoking `ansible-playbook`/`ansible` programmatically with structured output, event streaming, and
artifact capture. Actively maintained (2.4.3, March 2026) and the officially recommended way to embed
Ansible execution into a custom Python app ([PyPI](https://pypi.org/project/ansible-runner/)). Relevant
to this project only as a **building block**: if the "thin glue" the project goals call for turns out to
need a custom job-runner (e.g., a small Go/Python service gluing Alertmanager webhooks → playbook runs →
job history in SQLite), `ansible-runner` is the correct primitive to shell out to rather than hand-rolling
`subprocess.run(["ansible-playbook", ...])`. It provides no UI, no scheduler, no inventory management, no
job history persistence, and no vault UI on its own — all of that is DIY.

### Ansible Navigator
A TUI for authoring, running, and troubleshooting playbooks/inventories interactively, built on top of
`ansible-runner` and (optionally) containerized "execution environments." Actively maintained
([GitHub](https://github.com/ansible/ansible-navigator), deep-dive current as of May 2026). It is a
**developer-facing interactive tool**, not an orchestration service — no persistent job history, no
scheduler, no webhook/trigger surface, no multi-user API. It's a good fit for the "TUI-first" *operator
console* half of this project's UI priority (a pleasant terminal experience for running and inspecting
playbooks by hand) but does not itself satisfy the "AWX/Terraform-Cloud-for-homelab" orchestration
requirement — it would sit alongside whatever holds job history/scheduling/triggers, not replace it.

### systemd timers + `ansible-pull`
The genuinely single-binary answer, because there is no orchestrator binary at all: each managed host
runs `ansible-pull` on a systemd timer, pulling its own playbook/inventory from a Git repo and applying
it locally. This is a real, actively-discussed pattern for homelabs and edge/intermittently-connected
fleets in 2025-2026 ([DEV Community writeup](https://dev.to/lyraalishaikh/from-push-to-pull-gitops-style-linux-automation-with-ansible-pull-systemd-timers-490a),
[systemd timer gist](https://gist.github.com/jaepetto/433ac11d13cd2b693178ebe16c72bd70)). It is extremely
lightweight (Ansible core only, no server), self-healing (a host that comes back online catches up on
its own), and pairs naturally with git-based backup/DR (the source of truth already lives in a repo, which
maps directly onto this project's "backup to a GitHub repo" goal).

Its ceiling is real, though: there is no central job history beyond `journalctl` per host, no central
inventory or dashboard, no built-in secret store (vault password distribution to every pulling host is
your problem), no plan/apply workflow for Terraform, and no natural place to hang webhook-triggered
runbooks (Alertmanager can trigger a *pull* only indirectly — e.g. by touching a file/branch a host polls,
or by SSHing in to force a run — there's no push path by design). This pattern is a strong complement to a
central orchestrator (e.g., use it for baseline config drift-correction on every host, while Semaphore
owns one-off runbooks, Terraform plans, and alert-triggered actions) rather than a full substitute for one.

---

## Integration notes against the project's goals

- **Combines AWX-Tower-like + Terraform-Cloud-like roles in one process:** Only **Semaphore UI** does
  this natively today — Ansible + Terraform/OpenTofu/Terragrunt/Pulumi/Bash task types, one binary, one
  job-history/API surface. AWX has no Terraform story at all (it's Ansible-only); Rundeck can shell out to
  `terraform` via a script step but has no IaC-aware plan/apply UX.
- **ansible-vault:** Semaphore's Key Store handles vault passwords natively and attaches them per
  template ([docs](https://semaphoreui.com/docs/user-guide/key-store)). AWX has equivalent (arguably more
  mature) credential-type handling, but the project is stalled. Rundeck has no native vault concept.
- **sops/age/OpenBao-class secrets:** None of these tools have first-class sops/age integration out of
  the box. Semaphore's free tier encrypts its own secret store at rest with an operator-supplied key,
  which is a reasonable baseline; OpenBao/Vault-class external secret backends are gated behind
  Semaphore's Pro tier. If sops/age is a hard requirement, plan on it living *outside* the orchestrator
  (encrypt secrets in the git repo with sops/age, decrypt just-in-time as a playbook/task step) regardless
  of which platform is chosen — this is the more homelab-appropriate pattern anyway and avoids paying for
  Semaphore Pro's Vault integration.
- **Backup of state/config/secrets to NAS/S3/GitHub:** Semaphore's single embedded BoltDB file (or a
  Postgres dump) is trivial to snapshot to any of the three targets named in the project goals; the CLI
  gained project export/import in v2.17 ([release notes](https://semaphoreui.com/releases/semaphore-v2_17)),
  which is a clean building block for a DR script. AWX's backup surface is Postgres + Kubernetes secrets,
  meaningfully harder to snapshot/restore cleanly for a one-operator setup. `ansible-pull` has the
  simplest DR story of all — the state *is* the git repo, already an explicit backup target in the
  project goals.
- **Event-driven triggers (Alertmanager/ntfy/webhook → runbook):** Semaphore's inbound integration
  system (webhook URL + matcher + extractor → task template) is a good fit and is available in the free
  tier ([integrations docs](https://semaphoreui.com/docs/user-guide/integrations)). AWX has equally
  capable webhooks but carries the whole-project-risk noted above. Rundeck's basic "Run Job" webhook
  handler is Community-tier and workable, but the richer "Advanced Run Job" handler is paywalled.
  `ansible-pull` has no native inbound trigger path.
- **TUI-first UI priority:** None of the orchestrator platforms (AWX/Semaphore/Rundeck) have a real TUI —
  all are web-first. `ansible-navigator` is the one genuinely TUI-native tool in this list and is worth
  keeping in the operator's toolbox for ad hoc/interactive runs regardless of which orchestrator holds
  history and scheduling; it doesn't compete with Semaphore, it complements it.
- **Bootstrap/DR simplicity:** Semaphore's "single binary/container, BoltDB, minutes to stand up" story
  ([virtualizationhowto.com](https://www.virtualizationhowto.com/2025/07/ansible-semaphore-ui-my-new-go-to-infrastructure-automation-tool/))
  is the closest match to "trivially easy to bootstrap and recover" of anything surveyed. AWX's
  Kubernetes-operator bootstrap and Rundeck's JVM/Postgres bootstrap are both meaningfully more moving
  parts to recreate from a cold backup.

---

## Recommendation

**Use Semaphore UI (MIT core, self-hosted) as the orchestration hub, complemented by plain
`ansible-pull` + systemd timers for baseline drift-correction on individual hosts, with `ansible-navigator`
kept as the interactive/TUI tool for ad hoc runs.**

Rationale:

1. Semaphore is the only tool surveyed that is simultaneously *lightweight* (single Go binary or
   container, embedded BoltDB, no separate database/queue required), *actively maintained* (releases
   through Feb 2026 with real feature velocity), and *already scoped to this project's exact combined
   requirement* (Ansible + Terraform/OpenTofu in one job-history/API/scheduler/webhook surface).
2. AWX is disqualified for a **new** homelab build in 2026 specifically because of project health, not
   features — it has been effectively frozen since mid-2024 with no release timeline, despite having the
   deepest feature set of anything surveyed. Revisit if/when the refactor ships and a new release lands.
3. Rundeck is heavier (JVM footprint) and more general-purpose than this project needs, with its more
   useful webhook handling gated behind a commercial Enterprise tier; it's a reasonable choice for
   polyglot job orchestration beyond Ansible/Terraform, but not the best fit here.
4. Budget for **Semaphore Pro ($490/yr)** as an optional upgrade only if the Terraform-Cloud-replacement
   piece (hosted state + locking) or Vault-backend secrets become load-bearing — the free tier is
   sufficient to start, and Terraform state can be pointed at a self-hosted S3-compatible backend
   (already a project goal) without needing Semaphore's paid HTTP backend at all.
5. Layer `ansible-pull`/systemd timers underneath for continuous drift-correction on hosts that should
   self-heal even if the Semaphore instance itself is down — this also gives the "recover from a backup"
   goal a fallback path (git repo → any host can re-converge itself independent of the orchestrator's
   state), and dovetails with the project's planned GitHub-repo backup target.
6. Keep `ansible-runner` in mind as the glue library if a custom event-driven action (e.g., a bespoke
   Alertmanager-to-runbook bridge beyond what Semaphore's webhook matcher/extractor can express) is ever
   needed — it's the standard, actively-maintained primitive for that, not something to hand-roll.

## Sources

- [AWX Operator sizing requirements](https://ansible-awx.mintlify.app/installation/requirements)
- [AWX Kubernetes hardware requirements forum thread](https://forum.ansible.com/t/what-are-the-hardware-requirements-for-deploying-awx-in-kubernetes/43526)
- [Is there a future for AWX? — Ansible Forum](https://forum.ansible.com/t/is-there-a-future-for-awx/44527)
- [AWX webhooks documentation (24.6.1)](https://docs.ansible.com/projects/awx/en/24.6.1/userguide/webhooks.html)
- [Semaphore UI GitHub repository](https://github.com/semaphoreui/semaphore)
- [Semaphore UI Docker image](https://hub.docker.com/r/semaphoreui/semaphore)
- [Semaphore v2.16 release notes](https://semaphoreui.com/releases/semaphore-v2_16)
- [Semaphore v2.17 release notes](https://semaphoreui.com/releases/semaphore-v2_17)
- [Semaphore Key Store docs (ansible-vault handling)](https://semaphoreui.com/docs/user-guide/key-store)
- [Semaphore Integrations/webhooks docs](https://semaphoreui.com/docs/user-guide/integrations)
- [Semaphore Pro pricing/features](https://semaphoreui.com/pro)
- [Semaphore Terraform/OpenTofu HTTP backend (Pro)](https://semaphoreui.com/docs/user-guide/task-templates/apps/terraform/states)
- [AWX vs Semaphore UI — Semaphore's own comparison](https://semaphoreui.com/blog/awx-vs-semaphore)
- [Ansible Semaphore UI writeup — virtualizationhowto.com](https://www.virtualizationhowto.com/2025/07/ansible-semaphore-ui-my-new-go-to-infrastructure-automation-tool/)
- [Semaphore command-center writeup — blog.alphabravo.io](https://blog.alphabravo.io/the-ultimate-command-center-managing-terraform-and-ansible-with-semaphore-ui/)
- [Rundeck open source page](https://www.rundeck.com/open-source)
- [Rundeck Docker configuration reference](https://docs.rundeck.com/docs/administration/configuration/docker.html)
- [Rundeck webhooks how-to](https://docs.rundeck.com/docs/learning/howto/using-webhooks.html)
- [Rundeck GitHub repository](https://github.com/rundeck/rundeck)
- [ansible-runner PyPI page](https://pypi.org/project/ansible-runner/)
- [ansible-runner GitHub releases](https://github.com/ansible/ansible-runner/releases)
- [ansible-navigator GitHub repository](https://github.com/ansible/ansible-navigator)
- [ansible-pull + systemd timers pattern — DEV Community](https://dev.to/lyraalishaikh/from-push-to-pull-gitops-style-linux-automation-with-ansible-pull-systemd-timers-490a)
- [Sample ansible-pull systemd timer unit — gist](https://gist.github.com/jaepetto/433ac11d13cd2b693178ebe16c72bd70)
