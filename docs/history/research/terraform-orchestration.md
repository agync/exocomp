# Self-hosted Terraform/OpenTofu orchestration for a homelab control stack

Research date: 2026-08-21. Scope: run/plan/apply orchestration, state storage + locking +
encryption, and drift detection for OpenTofu/Terraform at homelab scale (one operator, a
handful of hosts) — evaluated against the control-stack goals of single-container/single-binary
deployment, TUI-first UI, easy disaster recovery, and composing existing tools over building new
ones.

## TL;DR

- **License first**: Terraform itself has been BSL (non-OSI, "no competing product") since
  August 2023; **OpenTofu is the MPL-2.0, CNCF-hosted continuation** and is the only sane base
  for anything self-built or redistributed in this space. Prefer OpenTofu-compatible tooling.
  [opentofu.org](https://opentofu.org/docs/v1.7/intro/whats-new/), [Encore: OpenTofu vs
  Terraform 2026](https://encore.dev/articles/opentofu-vs-terraform-2026)
- **State storage/locking/encryption is now a solved problem without any extra service**:
  OpenTofu 1.7+ has native, backend-agnostic state/plan encryption (PBKDF2 passphrase, or
  AWS/GCP/Azure KMS, or OpenBao Transit — the last two still experimental); Terraform/OpenTofu
  1.11+'s S3 backend does native conditional-write locking with **no DynamoDB**. Point this at
  any S3-compatible bucket and you have encrypted, locked remote state with zero orchestration
  daemon. Caveat below on MinIO.
- **Run orchestration ("the AWX/Terraform-Cloud part") is the part with no small, obviously-right
  answer.** Every dedicated orchestrator in this space is heavier than the project's "single
  container" bar: Terrakube is a multi-service Java/Spring platform (Postgres + object storage +
  Keycloak-class auth + a UI + an executor), Burrito requires a Kubernetes cluster, and OTF is a
  solo-maintainer pre-1.0 project needing Postgres. The one option that is genuinely
  single-binary/single-container is **Atlantis** — but Atlantis is PR-comment-driven CI glue,
  not a scheduler/inventory/job-history system; it deliberately does not store state itself.
- **Recommendation for this project**: don't adopt a dedicated TACoS (Terraform Automation
  Collaboration Software) platform. Compose **OpenTofu + S3-compatible backend with native
  encryption/locking** for state, and build the thin "run orchestration" layer (schedule, job
  history, drift-check cron, webhook triggers) as glue in the control stack itself, optionally
  fronted by **Atlantis** if PR-driven apply review is wanted, or by **leg100/pug** as a
  TUI-first interactive driver for ad hoc runs. This matches the project's own stated bias
  ("build thin glue only where needed") better than any of the packaged platforms do.

---

## Landscape table

| Name | Language/runtime | License | Footprint | Single binary/container? | Health (2026) |
|---|---|---|---|---|---|
| **Atlantis** | Go | Apache-2.0 | One Go binary/container; default locking is embedded BoltDB (file on disk), no DB required for single-node use | **Yes** (single-node, no external DB) | Active; Apache-governed, releases through June 2026, healthy issue/PR throughput [GitHub](https://github.com/runatlantis/atlantis) |
| **Terrakube** | Java/Spring Boot (API, UI, registry, executor as separate services) | Apache-2.0 | Multi-service: Postgres, S3/MinIO object storage, identity provider, executor pods; Docker Compose or Helm | No — several containers | Active; ~900 stars, releases into mid-2026, HashiCorp confirmed BSL-compatible use [docs.terrakube.io](https://docs.terrakube.io/), [platformengineering.org](https://platformengineering.org/tools/terrakube) |
| **Burrito** | Go, Kubernetes operator (CRDs) | Apache-2.0 | Requires a Kubernetes cluster; runs Terraform in ephemeral runner pods | No — needs K8s, not a standalone binary | Active, pre-1.0 (breaking changes expected); ~750 stars, named maintainers, conference talks in 2025 [GitHub](https://github.com/padok-team/burrito) |
| **tofu-controller** (flux-iac, formerly Weave TF-Controller) | Go, Flux/Kubernetes controller | Apache-2.0 | Requires Flux + Kubernetes | No | Active but niche; moved from Weaveworks to the flux-iac community org in 2024, commits into 2025 [GitHub](https://github.com/flux-iac/tofu-controller) |
| **OTF** (leg100/otf) | Go | MPL-2.0 (Terraform-Enterprise-alternative semantics; author states BSL-compatible use) | Single `otfd` binary, but **requires PostgreSQL** | Binary yes, but not self-contained (needs Postgres) | Active solo-maintainer project, still pre-1.0 (v0.5.x as of Feb 2026); small community [GitHub](https://github.com/leg100/otf) |
| **leg100/pug** | Go | — | Single Go binary, talks to local `terraform`/`tofu` CLI; no server component at all | **Yes**, and it's the closest thing to "TUI-first" for this domain | Active; same author as OTF, purpose-built TUI, integrates infracost |
| Scalr / env0 / Spacelift (self-hosted tiers) | Proprietary SaaS control planes, some with self-hosted "agents" or on-prem builds | Proprietary | Self-hosted variants are enterprise-grade installs (weeks-to-months setup per vendor docs); hosted free tiers exist (Scalr 50 runs/mo, Spacelift 2 users/1 worker, env0 250 runs/mo) but are cloud SaaS, not self-hosted | No | Actively developed commercial products; wrong shape for a homelab (SaaS dependency or enterprise install) |
| Plain OpenTofu + encrypted S3-compatible backend | Go (the `tofu` CLI itself) | **MPL-2.0** | Just the `tofu` binary + object storage you already run | Yes — no orchestrator at all, by design | OpenTofu is CNCF-hosted (accepted April 2025), current stable ~v1.12.x as of mid-2026, fast-moving [Wikipedia/OpenTofu](https://en.wikipedia.org/wiki/OpenTofu) |
| HTTP state backend implementations (terrastate, tfstate.dev) | Go / serverless | Open source (terrastate), free hosted service (tfstate.dev) | Terrastate: single small binary/container. tfstate.dev: no self-host, it's a hosted GitHub-token-gated service | Terrastate: yes | Both low-activity niche projects; terrastate has infrequent commits; tfstate.dev is a convenience hosted service, not something to depend on for DR-critical state [tfstate.dev](https://tfstate.dev/), [terrastate](https://github.com/webhippie/terrastate) |

---

## Per-option analysis

### Terraform vs OpenTofu — the license question that gates everything else

HashiCorp relicensed Terraform from MPL-2.0 to the Business Source License (BSL) in August 2023;
the BSL forbids offering Terraform "in a competitive way" against HashiCorp's own commercial
products, and each release only converts to MPL four years after that release. OpenTofu is the
fork of the last MPL-licensed Terraform, governed by the Linux Foundation, and was accepted into
the CNCF in April 2025 (with a license exception letting it keep MPL-2.0 rather than relicense to
Apache-2.0). IBM acquired HashiCorp in 2025, which hasn't changed the license but adds long-term
roadmap uncertainty on the Terraform side. By 2025–2026 OpenTofu has meaningfully diverged with
features Terraform doesn't have (native state encryption, provider-defined functions), so for a
personal/homelab tool there is no real reason to prefer Terraform CLI over OpenTofu — OpenTofu is
free of BSL ambiguity and gets the encryption feature this project specifically wants.
[opentofu.org 1.7 release notes](https://opentofu.org/docs/v1.7/intro/whats-new/),
[Encore: OpenTofu vs Terraform 2026](https://encore.dev/articles/opentofu-vs-terraform-2026),
[Jorijn Schrijvershof: the fork finally diverged](https://jorijn.com/en/blog/opentofu-vs-terraform-2026-the-fork-finally-diverged/)

### State storage, locking, encryption — solvable without a platform

**Encryption.** OpenTofu 1.7 (April 2024) shipped native, backend-agnostic state and plan
encryption: state is encrypted client-side before it ever reaches the backend, so the backend
itself doesn't need to be trusted. Key providers include a PBKDF2 passphrase (no external
service needed — a good homelab default), and AWS/GCP/Azure KMS or an **OpenBao Transit engine**
key provider (still marked experimental as of the OpenTofu team's own tracking issue). A
separate community project, `tofu-age-encryption`, adds an age-based (asymmetric) external
encryption method as an alternative to symmetric passphrase workflows.
[OpenTofu state encryption docs](https://opentofu.org/docs/language/state/encryption/),
[OpenBao key provider issue #1174](https://github.com/opentofu/opentofu/issues/1174),
[tofu-age-encryption](https://github.com/josh/tofu-age-encryption)

**Locking.** Terraform/OpenTofu 1.11+ (GA; experimental from 1.10) added native S3-backend state
locking via conditional writes (`use_lockfile = true`), writing a `.tflock` object using the
`If-None-Match: *` HTTP header — no DynamoDB table required, fewer moving parts, fewer IAM
permissions. **Important caveat for homelab S3-compatible storage**: MinIO's conditional-write
implementation does not accept the `*` wildcard etag that this locking mechanism requires (it
only accepts exact etags), so **native lockfile locking does not currently work against MinIO**.
RustFS (a Rust MinIO-alternative) claims closer S3 conditional-write compatibility but has its
own ETag-quoting interoperability bugs reported in 2026; Garage doesn't support conditional PUT
at all. Practical implication: if self-hosting object storage for state, either keep using
DynamoDB-style locking is not an option either (that's AWS-only) — the safe defaults are (a) an
actual S3-compatible provider that supports the real conditional-write semantics (e.g. a cheap
real AWS S3 bucket, Cloudflare R2, or Backblaze B2 — verify each against the same wildcard
requirement before relying on it), or (b) accept MinIO for storage but don't rely on its locking
guarantee, and instead serialize applies at the orchestration layer (a single control-stack
process that only ever runs one apply at a time is sufficient at homelab scale and sidesteps the
problem entirely). [S3 native locking PR](https://github.com/hashicorp/terraform/pull/35661),
[bschaatsbergen: S3 native state locking](https://www.bschaatsbergen.com/s3-native-state-locking),
[MinIO conditional-write incompatibility issue #20346](https://github.com/minio/minio/issues/20346),
[RustFS ETag issue #1458](https://github.com/rustfs/rustfs/issues/1458)

**HTTP backend implementations.** `terrastate` (webhippie) is a small self-hostable Go
implementation of Terraform's generic HTTP backend protocol with locking — a legitimate
lightweight option if S3-compatible conditional writes turn out to be unreliable, but it's a
low-activity niche project and adds a service to operate and back up. `tfstate.dev` is a free
*hosted* HTTP backend gated by a GitHub token — convenient for quick experiments, unsuitable as a
dependency for something the project wants to be able to disaster-recover independently (state
custody left the homelab). Given native OpenTofu backends already work, there's little reason to
add either.

### Atlantis — the only genuinely single-binary orchestrator here

Atlantis is Apache-2.0, written in Go, ships as one binary/container, and its default locking
store is an embedded BoltDB file — no external database needed for a single-node deployment
(Redis is only needed if you want multiple Atlantis instances for HA, irrelevant at homelab
scale). It listens for VCS webhooks (GitHub/GitLab/Bitbucket) and runs `plan`/`apply` in response
to PR comments, posting output back to the PR. This is a genuinely good fit for the project's
footprint constraint, but it solves a narrower problem than "Terraform Cloud for homelab": it has
**no built-in state storage** (state still lives wherever your backend config points, e.g. the
OpenTofu-native encrypted S3 setup above), **no scheduling/cron**, and **no job history UI**
beyond PR comment threads — its job history is whatever your VCS retains. It's best understood as
a thin apply-gate glued to your existing VCS + backend, which lines up well with "compose
existing tools, build thin glue only where needed" — but a homelab operator running solo (no PR
review process) gets comparatively little from Atlantis's core value proposition (human review
gate on PRs) and would be using it mainly as a plan/apply runner with a webhook trigger.
[runatlantis.io](https://www.runatlantis.io/docs/server-configuration), [Atlantis locking —
DeepWiki](https://deepwiki.com/runatlantis/atlantis/5.1-locking-and-concurrency-control)

### Terrakube — closest to actual Terraform-Enterprise parity, heaviest footprint

Terrakube explicitly targets replacing Terraform Enterprise/Scalr/env0: remote runs, a visual
state UI, private module/provider registries, RBAC/SSO, scheduled jobs (usable for drift
detection via a scheduled plan + Rego policy that flags non-zero diffs, with Slack
notification), and GitHub status-check integration. It's Apache-2.0 and HashiCorp has
acknowledged it as BSL-compliant. The catch is architecture: it's a Java/Spring Boot platform
with separate API, UI, registry, and executor services, needing Postgres and S3-compatible
object storage at minimum, typically deployed via Helm to Kubernetes (Docker Compose is
supported for smaller setups but is still multiple containers with a JVM footprint). This is
real Terraform-Cloud-class functionality, but it is not "single container," and running a JVM
platform for a homelab of a handful of hosts is disproportionate. [Terrakube
docs](https://docs.terrakube.io/), [Terrakube drift detection
guide](https://docs.terrakube.io/user-guide/drift-detection)

### Burrito and tofu-controller — Kubernetes-native, wrong shape for this project

Both are Kubernetes operators ("Argo CD for Terraform"): they reconcile Terraform/OpenTofu
resources declared as CRDs, run plans in ephemeral runner pods, and (Burrito specifically)
continuously re-plan to detect and optionally auto-apply drift. Both are Apache-2.0 and actively
maintained (Burrito ~750 stars, named maintainers, conference presence in 2025; tofu-controller
moved from Weaveworks to the community-run flux-iac org in 2024 and continues to receive commits
into 2025). Neither is usable without an existing Kubernetes cluster, which rules them out for a
project whose deployment bar is single-container/single-binary — unless the control stack itself
already assumes a K8s substrate, which the stated goals don't suggest. [Burrito
docs](https://docs.burrito.tf/latest/guides/iac-drift-detection/), [tofu-controller
GitHub](https://github.com/flux-iac/tofu-controller)

### OTF (leg100/otf) and leg100/pug — small, honest, worth knowing about

OTF aims to be a from-scratch open-source Terraform Enterprise alternative (SSO, teams, agents,
no per-resource pricing), written in Go with a single `otfd` daemon binary — but it requires
PostgreSQL, and as of February 2026 it's still pre-1.0 (v0.5.14), maintained essentially by one
developer (Louis Garman, "leg100"). It's a legitimate lightweight-ish option if the project ever
wants a real multi-user run orchestrator, but "solo-maintainer pre-1.0" is a maintenance-health
flag worth weighing against building the equivalent glue in-house.

The more interesting artifact from the same author is **`leg100/pug`** — a terminal UI for
driving Terraform/OpenTofu directly: parallel plan/init/apply across workspaces, interactive
state management (move/delete/target), infracost integration, no server component at all, single
Go binary. Given this project's explicit **TUI-first** UI priority, `pug` (or its design ideas)
is a much closer match to "what a homelab Terraform Cloud replacement's front end should feel
like" than any of the web-UI-first platforms above — it's worth evaluating directly as either a
bundled interactive tool or a design reference for the control stack's own TUI.
[leg100/pug](https://github.com/leg100/pug)

### Commercial self-hosted tiers (Scalr, env0, Spacelift) — not a fit

All three offer generous free *hosted* tiers in 2026 (Scalr: 50 runs/mo; Spacelift: 2 users/1
public worker; env0: 250 runs/mo, 30 environments as of August 2026) but these are SaaS control
planes, not self-hosted software — using them means depending on a third party for
plan/apply/state custody, which conflicts with the project's disaster-recovery-from-backup goal.
Their actual self-hosted/on-prem offerings are enterprise products with correspondingly heavy
install processes (documented as "weeks to months" of setup), aimed at organizations, not a
one-operator homelab. Not recommended at any tier for this project. [Scalr: self-hosted
migration options](https://scalr.com/learning-center/self-hosted-terraform-options-for-tfe-spacelift-scalr)

---

## Integration notes relative to the project goals

- **Secret management (ansible-vault / sops / age / OpenBao-class)**: OpenTofu's native state
  encryption with a PBKDF2 or age-based key provider is a strict improvement over "hope the
  backend is secure," and composes cleanly with a broader sops/age secrets strategy — the same
  age keypair used for sops-encrypted Ansible secrets can plausibly back OpenTofu's `tofu-age-encryption`
  provider, giving one key-management story across both tools. OpenBao Transit as a key provider
  is real but still experimental upstream; don't build DR-critical infrastructure on it yet.
- **Backup primitives (NAS/S3/GitHub)**: because OpenTofu state encryption happens client-side,
  an encrypted state object is safe to replicate anywhere — NAS via SMB/NFS, any S3-compatible
  provider, or even committed to a private Git repo (the ciphertext is opaque). This means the
  project's planned generic "encrypted backup of state/config/secrets" mechanism can treat
  OpenTofu state as just another file to sweep up, with no Terraform-specific backup logic
  needed — a good sign that composing rather than building here is the right call.
- **Locking discipline**: given the MinIO conditional-write gap above, if the project's chosen
  NAS/S3-compatible target is MinIO, either (a) verify current MinIO behavior before depending on
  `use_lockfile`, or (b) have the control stack's own run orchestrator serialize applies (trivial
  at homelab scale — one worker, one queue) rather than depending on backend-level locking at
  all. This removes the dependency on any orchestrator's built-in locking mechanism entirely.
- **Reactive/event-driven runs (Alertmanager, healthchecks, ntfy)**: none of the dedicated
  Terraform orchestrators surveyed have first-class alerting-webhook-triggered apply support out
  of the box — Atlantis triggers off VCS webhooks specifically, Terrakube triggers off its own
  scheduler/VCS, Burrito/tofu-controller trigger off Kubernetes reconciliation loops. A generic
  "receive an Alertmanager/ntfy webhook, resolve it to a named runbook or `tofu apply` job" layer
  is squarely in the thin-glue-to-build category for this project, not something to source
  off-the-shelf from the Terraform ecosystem.
- **Run history / inventory / scheduling ("the AWX part")**: this is the actual gap. Nothing
  surveyed cleanly gives job history + scheduling + drift cron in a single binary without either
  a JVM platform (Terrakube) or a Kubernetes cluster (Burrito/tofu-controller) or a Postgres
  dependency (OTF). The pragmatic path is a minimal run-log (even a SQLite table or an
  append-only file the control stack already needs for Ansible job history) shared between the
  Ansible and Terraform sides of the stack, rather than adopting a Terraform-specific run
  history platform.

## Recommendation

For this project's homelab scale and single-container bias, **do not adopt a packaged TACoS
platform**. Instead:

1. **Runtime**: standardize on **OpenTofu** (MPL-2.0, CNCF-governed), not Terraform CLI — avoids
   BSL ambiguity entirely and is the only one with native state encryption.
2. **State backend**: an S3-compatible bucket (verify real conditional-write support, or a NAS
   SMB/NFS-backed store) with OpenTofu's **native state encryption** (PBKDF2 passphrase or an
   age-based key provider shared with the rest of the stack's secrets story) turned on. Don't
   depend on backend-native locking if the storage target is MinIO — serialize applies in the
   control stack's own scheduler instead.
3. **Run orchestration**: build the thin layer yourself — job queue/history, cron-based drift
   checks (`tofu plan -detailed-exitcode` on a schedule, diff a summary, alert if non-empty),
   and a webhook receiver for Alertmanager/ntfy-triggered runs — sharing infrastructure with
   whatever equivalent layer the project builds for Ansible. This is squarely "thin glue," per
   the project's own preference, and every off-the-shelf option surveyed is disproportionately
   heavy (Terrakube), infrastructure-dependent (Burrito/tofu-controller need K8s; OTF needs
   Postgres), or narrower in scope than needed (Atlantis has no scheduler or job history of its
   own).
4. **Optional bolt-ons, not the core**:
   - **Atlantis** if/when PR-driven apply review becomes valuable (e.g. a second operator, or
     just wanting a VCS-comment audit trail) — it's genuinely single-binary and composes with the
     backend above without owning state itself.
   - **leg100/pug** as an interactive TUI front-end for ad hoc `tofu` work, and as a design
     reference for the control stack's own TUI given the project's TUI-first priority.
