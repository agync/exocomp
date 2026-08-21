# Bootstrap & Recovery Patterns for a Homelab Control Stack

Research note for the homelab "control stack" project: a lightweight, single-operator
replacement for AWX + Terraform Cloud that must be trivial to bootstrap, back up, and
disaster-recover. This note surveys how existing single-binary / small-footprint projects
solve bootstrap, secrets, backup, and self-management, and closes with concrete
recommendations for this project's design.

Research date: 2026-08-21. Version numbers and star counts below are snapshots as of that date.

---

## 1. Landscape table

| Project | Role / relevance | Language | License | Footprint | Single binary? | Health (2026) |
|---|---|---|---|---|---|---|
| [PocketBase](https://pocketbase.io/) | Reference pattern: Go + embedded SQLite + built-in backup/restore + admin UI | Go | MIT | ~1 binary, SQLite file(s) | Yes | Active — v0.39.11 (2026-08-14), ~60.7k stars, single maintainer |
| [Gitea](https://about.gitea.com/) | Reference pattern: single-binary `dump`/restore, SQLite-capable | Go | MIT | 1 binary + data dir | Yes (SQLite mode) | Active — v1.26.1 (Apr 2026), large community, forked from Gogs |
| [Headscale](https://headscale.net/) | Reference pattern: single-binary control-plane server, SQLite state, Litestream-friendly | Go | BSD-3-Clause | 1 binary + SQLite | Yes | Active — updated 2026-07-29, steady release cadence |
| [Litestream](https://litestream.io/) | Continuous SQLite → object-storage replication, point-in-time restore | Go | Apache-2.0 | 1 sidecar binary | Yes | Active — 0.5.x line, follow-mode/crash-resume added late 2025 |
| [Semaphore UI](https://github.com/semaphoreui/semaphore) | Closest AWX analog: runs Ansible/Terraform/OpenTofu/Terragrunt jobs, schedules, inventories, single binary or container | Go | MIT | 1 binary + SQLite/MySQL/Postgres | Yes (SQLite backend supported) | Active — regular releases; AWX itself has had no release since 2024 |
| [Terrakube](https://github.com/AlfonsoRR/terrakube) | Closest Terraform Cloud analog: remote runs, state UI, registries | Java (Spring Boot) + Angular | Apache-2.0 | Multi-service, K8s/Helm-oriented | No | Active but small community (~900 stars); heavier than homelab scale wants |
| [Atlantis](https://www.runatlantis.io/) | PR-driven Terraform/OpenTofu automation, no state management of its own | Go | Apache-2.0 | 1 binary | Yes | Active, widely used, but scoped to plan/apply-on-PR, not a state/inventory store |
| [OpenBao](https://openbao.org/) | Vault-API-compatible secrets engine, Linux Foundation governed fork of Vault | Go | MPL-2.0 | 1 binary (server) | Yes | Active — v2.5.0 (Feb 2026); heavier than needed for single-operator secrets |
| [sops](https://github.com/getsops/sops) + [age](https://github.com/FiloSottile/age) | Encrypt-in-place secrets for YAML/JSON/ENV, git-diffable, age as the key backend | Go | MPL-2.0 (sops), BSD-3-Clause (age) | 2 small binaries | Yes (each) | Active, widely adopted (Mozilla SOPS + FiloSottile's age), low complexity |
| [age-plugin-yubikey](https://github.com/str4d/age-plugin-yubikey) | Hardware-backed age identity (root-of-trust on a YubiKey) | Rust | MIT/Apache-2.0 | 1 plugin binary | Yes | Active, niche but stable |
| [restic](https://restic.net/) | Encrypted, deduplicated backup to S3/SFTP/local/rest-server, mature snapshot model | Go | BSD-2-Clause | 1 binary | Yes | Active — v0.18.1 (Sep 2025), very mature, large community |
| [Kopia](https://kopia.io/) | Alternative to restic; content-addressable, has a small web UI | Go | Apache-2.0 | 1 binary (+ optional UI) | Yes | Active, smaller community than restic |
| [Dagu](https://github.com/dagu-org/dagu) | Single-binary, no-DB DAG/cron workflow engine — candidate for the "reactive runbook" trigger layer | Go | GPL-3.0 (core) | 1 binary, flat-file state | Yes | Active, fast-growing (renamed dagu-org→dagucloud in 2026) |
| [ntfy](https://ntfy.sh/) | Self-hosted push notifications (pub/sub over HTTP), good escalation primitive | Go | Apache-2.0 (server) | 1 binary | Yes | Active, large user base, simple REST API |
| [healthchecks](https://github.com/healthchecks/healthchecks) (self-hosted) | Dead-man's-switch monitoring + alert routing (Slack/ntfy/PagerDuty/etc.) | Python/Django | BSD-3-Clause | App + DB (Postgres/SQLite/MySQL) | No (needs a DB service, though SQLite mode is light) | Active, mature, well documented |
| [OpenTofu](https://opentofu.org/) | Terraform fork with **native state encryption** (1.7+, external key providers in 1.10) | Go | MPL-2.0 | 1 binary | Yes | Active, CNCF-adjacent governance, fast release cadence |
| [k3s](https://k3s.io/) | Not in-scope as infra, but its bootstrap-token pattern is a useful reference model | Go | Apache-2.0 | 1 binary | Yes | Active, reference only |

---

## 2. Per-option analysis

### 2.1 Single-binary Go + embedded SQLite (the "PocketBase pattern")

PocketBase, Gitea, Miniflux, and Headscale all converge on the same shape: a single Go
binary that embeds `modernc.org/sqlite` or `mattn/go-sqlite3`, keeps all durable state in
one `.sqlite` file (or a small `pb_data`/`data` directory containing the DB plus
uploaded-file blobs), and ships an admin UI or CLI in the same binary. This is directly
the shape recommended for this project's control-plane server.

- **PocketBase** is the most mature expression of "backup is a first-class API." As of
  v0.16+ it has built-in `Settings → Backups` that produce a zip of the SQLite file(s) plus
  uploaded files, storable locally or on S3, with cron-driven auto-backup registered at
  bootstrap (`BaseApp.registerAutobackupHooks()`). Restore is a documented, one-command
  operation from the dashboard or CLI. This is close to the "trivially easy to
  bootstrap/recover" bar this project wants.
  ([DeepWiki: Backup and Restore](https://deepwiki.com/pocketbase/pocketbase/6.3-backup-and-restore),
  [PocketBase going-to-production docs](https://pocketbase.io/docs/going-to-production/))

- **Gitea** shows the *lower* bar: a `gitea dump` command bundles DB + repos + config +
  attachments into one archive, but restore is manual (unzip, move files into place,
  re-import the DB dump, regenerate hooks) — there is deliberately no `gitea restore`
  command. Useful as a cautionary example: a backup command alone does not satisfy "easy
  recovery," you need the inverse command too.
  ([Gitea backup-and-restore docs](https://docs.gitea.com/administration/backup-and-restore/))

- **Headscale** keeps it minimal: SQLite database, stop-the-service-and-copy-the-file as
  the officially blessed backup method, and community docs pairing it with Litestream
  for continuous replication + `litestream restore -if-db-not-exists` for zero-touch
  recovery on a fresh host. This is essentially the exact pattern this project should
  adopt for its own state.
  ([Backup headscale using Litestream](https://luislavena.info/til/backup-headscale-using-litestream/),
  [Headscale upgrade docs](https://headscale.net/stable/setup/upgrade/))

**Takeaway:** embed SQLite, keep all state (job history, inventory, secrets-at-rest
ciphertext, schedules) in one file + one blob directory, and design the backup format so
restore is *also* one command, not just backup.

### 2.2 Litestream: continuous SQLite backup

Litestream tails the SQLite WAL and streams pages to S3-compatible object storage (or a
local/SFTP replica) continuously, giving point-in-time recovery with no changes to the
application beyond running the sidecar. The 2025 0.5.x line materially improved things:
`-f` (follow) mode gives continuously-updated read replicas, and a `-txid` sidecar file
enables crash-resume without a full re-restore.
([litestream.io](https://litestream.io/), [How it works](https://litestream.io/how-it-works/))

Critically for the "disaster recovery from a backup" goal, the restore path is designed
to be idempotent and safe-by-default: `litestream restore -if-db-not-exists
-if-replica-exists mydb.sqlite` — safe to put in a systemd `ExecStartPre=` so a fresh
host self-heals its DB from the last replica before the app ever starts. This is exactly
the "one-command restore" primitive the project goals ask for, and it's already the
community-documented pattern for Headscale.
([restore reference](https://litestream.io/reference/restore/))

Caveat: Litestream only replicates the SQLite file(s) — it does not know about
associated blob storage (uploaded files, git repos, etc.), so a full control-plane
backup still needs a second mechanism (restic/rclone) for anything living outside the DB.

### 2.3 Declarative config bootstrap (config file + idempotent init)

Two patterns recur:

1. **Environment-variable admin bootstrap with an explicit disable step** — Miniflux's
   `CREATE_ADMIN=1` + `ADMIN_USERNAME`/`ADMIN_PASSWORD` creates the first admin on boot,
   but is explicitly **not idempotent**: docs warn to unset `CREATE_ADMIN` after first run
   or it will create a duplicate admin on every restart. This is a real footgun worth
   avoiding in this project's design — prefer "create admin only if the users table is
   empty" logic in code, not an env flag the operator must remember to unset.
   ([Miniflux binary install docs](https://miniflux.app/docs/binary_installation.html))

2. **GitOps self-bootstrap** — Flux CD's `flux bootstrap` command is the strongest
   real-world answer to "what manages the thing that manages infra": it installs the
   controllers into the cluster *and* commits their own manifests to the Git repo they'll
   subsequently sync from, so the control plane's own definition becomes one more
   thing it manages, with Git (not the cluster) as the actual root of truth.
   ([control-plane.io: What is FluxCD](https://control-plane.io/posts/what-is-fluxcd/))

Combined recommendation: config should be a single declarative file (TOML/YAML) checked
for presence at startup; if config + DB are both absent, run a guided first-boot wizard
(TUI) that (a) generates the age root key, (b) writes the config file, (c) seeds the
SQLite DB — and every step must be safe to re-run (check-before-create, not
create-and-hope).

### 2.4 "Recovery seed" patterns (age keypair as root of trust)

The clearest analog to a "recovery kit" in this space isn't infra tooling, it's
credential-manager UX:

- **Bitwarden/1Password recovery codes**: generated once at 2FA-enrollment time, meant to
  be printed/stored offline, and used only to disable/bypass the normal auth path when
  it's unavailable — a single secondary secret that recovers access to the primary
  secret store. ([Bitwarden recovery code docs](https://bitwarden.com/help/two-step-recovery-code/))
- **Vaultwarden/Bitwarden "emergency kit"**: community pattern of exporting account
  email + master password + 2FA recovery code + emergency-access grants into one
  printable PDF, stored in a physical safe — explicitly because the vault itself cannot
  be used to recover access to the vault.
  ([Bitwarden Emergency Kit generator](https://github.com/desaianand1/Bitwarden-Emergency-Kit),
  [Bitwarden security readiness kit](https://bitwarden.com/resources/bitwarden-security-readiness-kit/))
- **age-plugin-yubikey**: lets an `age` identity be hardware-backed by a YubiKey PIV
  slot; age's native multi-recipient encryption means a secret can be encrypted to both
  the YubiKey-backed identity *and* a paper-backed fallback identity simultaneously, so
  losing the hardware key doesn't mean losing the data.
  ([YubiKey + age writeup](https://sumguy.com/yubikey-age-portable-encryption/))
- **k3s bootstrap token**: the cluster's join/server token doubles as the PBKDF2
  passphrase that encrypts "bootstrap data" (certs/keys) in etcd; docs are explicit that
  losing the token without a separate backup of it makes the datastore's bootstrap data
  unrecoverable even if the datastore itself survives. Good cautionary example: don't
  let the "master secret" live *only* inside the thing it protects.
  ([K3s token docs](https://docs.k3s.io/cli/token))

**Synthesis for this project**: generate a single `age` keypair as the root of trust at
first bootstrap. Everything else (sops-encrypted ansible/terraform secrets, encrypted
config backups, encrypted state snapshots) is encrypted to that public key (and,
optionally, to a second "paper backup" age identity whose private key is only ever
printed as a QR code / mnemonic-like string, never stored on any managed host). The
private key for day-to-day use can optionally live on a YubiKey via `age-plugin-yubikey`;
the paper identity is the actual disaster-recovery fallback, analogous to a Bitwarden
recovery code. The generated "recovery kit" artifact (printable) should contain: the age
recovery private key (or its hardware-backed equivalent's public key + serial), the
restic/S3 repository password, and the one-line restore command — mirroring the
Bitwarden/Vaultwarden emergency-kit shape.

### 2.5 docker-compose vs Podman Quadlet / systemd

For a single-host, single-container homelab deployment, the two real options are Compose
and Podman Quadlet (systemd-generated units from a declarative `.container`/`.volume`
file). 2025-2026 commentary converges on:

- **Compose**: simpler mental model, huge ecosystem, but containers live outside
  systemd's service model — invisible to `journalctl`, dependent on a running daemon,
  no native `systemctl status`.
- **Quadlet**: containers become native systemd units — `systemctl start/stop/status`,
  `journalctl -u`, boot ordering via `After=`/`Wants=`, automatic restart via
  `Restart=`, and rootless-by-default execution (a breakout gets the operator's UID, not
  root) — a meaningfully smaller blast radius for a box also holding root-of-trust key
  material.
  ([Quadlet vs Compose](https://oneuptime.com/blog/post/2026-03-18-choose-between-quadlet-docker-compose-systemd/view),
  [Quadlet homelab writeup](https://dev.to/alvarito1983/quadlet-the-podman-feature-that-finally-makes-sense-on-a-homelab-2m3))

For *this* project specifically — a single binary/container that must start reliably on
boot, restart on crash, and be inspectable with the same tools an operator already uses
for every other systemd unit on the box — Quadlet is the better fit than Compose. Compose
remains the better *documentation* format to ship as a fallback for people not on
Podman/systemd (e.g., Synology/TrueNAS app users), so shipping both a `.container`
Quadlet unit and a `docker-compose.yml` for the same image is reasonable.

### 2.6 Self-update mechanisms

Go binaries commonly self-update via `go-github-selfupdate`
([rhysd/go-github-selfupdate](https://github.com/rhysd/go-github-selfupdate)) or the
newer `creativeprojects/go-selfupdate` (used by `resticprofile`), which pull a JSON
manifest, verify a hash/signature, download the matching-arch binary, and replace
themselves in place.
([go-selfupdate](https://github.com/creativeprojects/go-selfupdate)) Tailscale instead
defers to the OS package manager that originally installed it — arguably the more
robust choice on a system that already has a package manager, since it keeps one
source of truth for "what version is installed" instead of a binary silently replacing
itself outside the package DB. ([Tailscale update docs](https://tailscale.com/docs/features/client/update))

For this project: prefer the Tailscale approach if distributing via a system package
manager (rpm/deb) or Quadlet image tag; if distributing as a raw binary, adopt
`creativeprojects/go-selfupdate`-style signed-manifest self-update, but make it
**opt-in and never automatic** for a control plane that manages other infrastructure —
an unattended bad self-update is a single point of failure for everything downstream.

### 2.7 The chicken-and-egg problem

This is a well-recognized problem in the wild, not unique to this project:

- Kubernetes itself has a documented "cloud controller manager chicken-and-egg problem"
  where components need the very API server they're supposed to help bootstrap.
  ([kubernetes.io blog](https://kubernetes.io/blog/2025/02/14/cloud-controller-manager-chicken-egg-problem/))
- The generalized framing: infrastructure tooling requires infrastructure to run on, and
  "infrastructure that exists today could not be created from scratch if needed" without
  some manual, out-of-band first step. ([Brett Weir: The Bootstrapping Problem](https://brettweir.com/blog/bootstrapping-problem/))
- Flux CD's answer is **self-hosting the control plane's own definition**: `flux
  bootstrap` installs itself into the cluster *and* commits its own manifests to the Git
  repo it will then reconcile from — the control plane becomes just another object it
  manages, and "what manages the control plane" becomes "the control plane's own first
  commit, applied once by a human, by hand."
  ([control-plane.io](https://control-plane.io/posts/what-is-fluxcd/))
- Ansible's answer for the very first host is **`ansible-pull`**: invert push→pull so the
  managed node clones a git repo and configures itself with no control node required at
  all for that first run (cloud-init installs git+ansible, then `ansible-pull` takes
  over) — self-healing thereafter via a cron/systemd-timer re-run.
  ([ansible-pull decentralized automation](https://oneuptime.com/blog/post/2026-02-21-ansible-pull-mode-decentralized-automation/view),
  [jpmens.net: pull instead of push](https://jpmens.net/2012/07/14/ansible-pull-instead-of-push/))

**Synthesis**: there is no way to fully eliminate a manual first step — every
self-managing system in this survey has one (Flux's one human-run `bootstrap` command,
k3s's one `--cluster-init` invocation, ansible-pull's one cloud-init snippet). The right
design goal isn't "zero manual steps," it's "exactly one manual step, and it's the same
step used for both first-install and disaster-recovery-from-scratch." Concretely: this
project's control plane should be able to manage *its own* deployment (the Quadlet unit,
its own config, its own scheduled backup job) as just another entry in its own
inventory/playbook set — "dogfood" itself as node zero — while the actual recovery
primitive for a totally dead host is intentionally *not* "run the control plane," it's a
single `curl | sh`-style bootstrap script (à la Tailscale's installer or k3s's
`install.sh`) that: installs the binary, restores config+DB from the encrypted backup
using the recovery-kit age key, and starts the Quadlet unit. That bootstrap script is the
answer to "what manages the control plane" — it's small, dependency-free, and
independently versioned from the control plane itself so it still works even if the
control plane's own code is what's broken.

### 2.8 Reactive / event-driven automation

Real-world patterns for "alert fires → runbook runs":

- **Event-Driven Ansible (EDA)** / `ansible-rulebook`: a webhook (or Alertmanager
  receiver, Kafka consumer, file watcher) source triggers rule matching against incoming
  events, which then runs a playbook — the closest upstream-Ansible-native answer to
  "reactive runbooks."
  ([EDA tutorial](https://computingforgeeks.com/event-driven-ansible-eda-tutorial/),
  [event-driven automation with Prometheus](https://anuragbhatia.com/post/2025/01/event-driven-automation-with-prometheus/))
- **Direct webhook-to-runner**: Alertmanager receiver → HTTP webhook → Semaphore UI's
  task-trigger endpoint (or a homegrown dispatcher) is the lighter-weight version many
  homelabbers actually run, at the cost of losing EDA's richer event-matching language.
  ([prom2tower](https://github.com/pja237/prom2tower) as one concrete example)
- **Safety guardrails called out repeatedly in the sources**: cooldown periods, daily
  execution caps, and full audit logging of every automated action — because a
  self-healing loop that can re-trigger itself is a well-known way to turn a minor alert
  into an outage.

`ntfy` is a good, minimal fit for the escalation/notification leg (single binary, simple
REST publish, first-class mobile apps) ([ntfy.sh](https://ntfy.sh/),
[GitHub - binwiederhier/ntfy](https://github.com/binwiederhier/ntfy)); self-hosted
`healthchecks` is a good fit for the "did the nightly backup/job actually run"
dead-man's-switch leg, though note it needs its own small DB service (SQLite mode keeps
it light, but it's not a single static binary the way ntfy is).
([healthchecks GitHub](https://github.com/healthchecks/healthchecks))

`Dagu` is worth calling out separately: single Go binary, no external DB (flat files for
run history), YAML DAGs, built-in scheduler, SSH/Docker/shell step types, and an
MCP server for agent-driven triggering — it overlaps meaningfully with "the runbook
execution + scheduling engine" half of this project's scope and could plausibly be
vendored/embedded rather than reimplemented.
([dagu-org/dagu README](https://github.com/dagucloud/dagu))

---

## 3. Integration notes relative to project goals

| Goal | Recommended building block(s) | Notes |
|---|---|---|
| Single-binary/container deploy | Go + embedded SQLite (PocketBase pattern) | Own binary embeds job runner + inventory + scheduler; avoid a separate DB service |
| Ansible + Terraform job runner, schedules, history | Semaphore UI (adopt/embed) or Dagu (embed as the execution engine) + thin Ansible/OpenTofu wrappers | Don't rebuild AWX's job engine from scratch; Semaphore already speaks Ansible + Terraform/OpenTofu/Terragrunt natively and is MIT-licensed, single-binary-capable |
| Terraform Cloud-style plan/apply + state + drift | OpenTofu CLI (not Terraform, for the license + native state encryption) driven by the runner above; state stored as encrypted blobs in the control-plane's own SQLite/blob store rather than a separate remote backend | Terrakube is the "real" TFC clone but is Java/K8s-shaped — too heavy for this project's footprint target |
| Secret management | `age` keypair as root of trust; `sops` for ansible-vault-equivalent + terraform var/state encryption; optional `age-plugin-yubikey` for hardware backing | OpenTofu 1.10's native state encryption (PBKDF2 or external key provider) can point directly at the same age/sops key material, avoiding a second secrets story for Terraform state specifically |
| Backup of state/config/secrets → NAS/S3/GitHub | Litestream (continuous, SQLite-native) for the hot path; restic (or Kopia) for full encrypted snapshots to S3-compatible/SFTP/NAS targets; a plain `git push` of the sops/age-encrypted config tree to a private GitHub repo as the third leg | Three independent backup targets, all writing the *same* age-encrypted bytes, satisfies "NAS, S3, and GitHub" without three separate encryption stories |
| Reactive automation | Alertmanager/ntfy/healthchecks webhooks → EDA-style rule matching → runbook execution via the embedded runner, with cooldown + audit log guardrails | Reuse `ansible-rulebook`'s event-source model conceptually even if the implementation is bespoke and thin |
| Bootstrap/recovery UX | A dependency-free shell installer (Tailscale/k3s `install.sh` pattern) as the *only* thing needed on a dead host; it restores from the encrypted backup using the recovery-kit age key and starts the Quadlet unit | This script must be versioned and tested independently of the control plane binary itself — see §2.7 |
| Recovery kit | One printable/exportable artifact generated at first bootstrap: age recovery private key (or hardware-key reference), backup-repo password, restore one-liner | Directly modeled on Bitwarden/Vaultwarden "emergency kit" and Tailscale/Bitwarden-style recovery codes |
| Deployment mechanism | Podman Quadlet unit as primary; docker-compose.yml shipped as a compatibility fallback | Quadlet's systemd integration (journald, systemctl, rootless) matters more at homelab scale than Compose's portability, but Compose costs little to also ship |
| Self-update | Manual/opt-in only; prefer OS-package-manager-driven updates if distributing as rpm/deb+Quadlet image tag, else signed-manifest self-update (`go-selfupdate`) gated behind an explicit flag | Never auto-update a system that manages other infrastructure unattended |

---

## 4. Recommendation

Build the control plane as **one Go binary with an embedded SQLite database**, following
the PocketBase/Gitea/Headscale shape directly — this is the single most load-bearing
architectural choice, since it's what makes "trivially easy to bootstrap and recover"
achievable at all. Concretely:

1. **State**: all control-plane state (inventory, schedules, job history, encrypted
   secret blobs, encrypted terraform state) lives in one SQLite file + one blob
   directory. Continuously replicate it with **Litestream** to at least one
   S3-compatible target; the restore path (`litestream restore -if-db-not-exists`) is
   the same command used both for routine recovery and for standing up a brand-new host.

2. **Root of trust**: generate a single **age** keypair at first-boot. Use it (directly,
   or via **sops**) to encrypt ansible-vault-equivalent secrets, OpenTofu state (via
   OpenTofu 1.10's native state-encryption + external key provider pointed at the same
   age identity), and the backup archives themselves. Offer optional YubiKey-backed
   day-to-day use via `age-plugin-yubikey`, with a paper/QR "recovery kit" identity — in
   the shape of a Bitwarden/Vaultwarden emergency kit — as the actual disaster-recovery
   fallback.

3. **Job/runbook execution**: don't reimplement an AWX-class job engine from scratch.
   Either embed **Semaphore UI**'s approach (it already wraps Ansible + OpenTofu +
   Terragrunt with schedules, inventories, and a REST API, MIT-licensed, single-binary
   deployable) as a vendored dependency, or embed **Dagu**'s DAG/cron engine as the
   generic execution layer and write thin Ansible/OpenTofu invocation wrappers on top.
   Either is materially less work than a bespoke scheduler.

4. **Backup fan-out**: write encrypted backup archives (config + DB snapshot + state) via
   **restic** to three targets in parallel — NAS (SFTP/rest-server over SMB/NFS mount),
   S3-compatible object storage, and a `git push` of the encrypted tree to a private
   GitHub repo. All three consume the same age-encrypted bytes, so there's one encryption
   story, not three.

5. **Reactive layer**: accept Alertmanager/healthchecks/ntfy webhooks, match against
   simple declarative rules (EDA-inspired but bespoke and thin), and invoke the same
   runbook-execution path used for scheduled/manual runs — with mandatory cooldown
   windows, daily execution caps, and an audit log entry per triggered run.

6. **Deployment**: ship a **Podman Quadlet** `.container`/`.volume` unit as the primary
   deployment artifact (systemd-native restart/logging/rootless execution), and a
   `docker-compose.yml` as a compatibility fallback for non-systemd or Compose-only
   environments (Synology, TrueNAS Apps, etc.).

7. **Bootstrap/DR script**: the actual answer to "what manages the control plane" is a
   small, dependency-free shell installer versioned *separately* from the control-plane
   binary — modeled on Tailscale's/k3s's `install.sh` — whose only job is: fetch the
   binary, restore state from the encrypted backup using the recovery-kit age key, start
   the Quadlet unit. This script is what a human runs by hand on a dead box; the control
   plane's own scheduler should never be a dependency of its own recovery.

8. **Self-update**: opt-in only, never automatic, using a signed-manifest scheme
   (`creativeprojects/go-selfupdate`-style) if distributed as a raw binary, or deferred
   to the OS package manager if distributed as rpm/deb.

No existing project already covers this exact combination (Ansible/Terraform runner +
Terraform-Cloud-style state + secrets + backup fan-out + reactive automation) at homelab
scale — a targeted search for one came up empty, and the closest analogs (Terrakube,
AWX, full Semaphore+Vault+restic stacks) are each individually heavier than this
project's footprint target. The plan above is "compose existing tools, build thin glue,"
consistent with the stated project preference.
