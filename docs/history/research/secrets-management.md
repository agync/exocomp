# Secrets Management for a Homelab Control Plane

Research note — 2026-08-21. Scope: candidates for secret-at-rest storage, key
management, and Ansible/Terraform (OpenTofu) integration inside a single-operator
homelab "control stack" that also runs playbooks, IaC applies, and reactive
runbooks. Evaluated against the project goals: single-binary/lightweight,
trivial bootstrap and disaster recovery, TUI-first, prefer composing existing
tools over building new ones.

---

## TL;DR

For a homelab control plane, the pragmatic stack is **sops + age as the
encryption-at-rest layer for everything (Ansible vars, Terraform/OpenTofu vars
and state, the control stack's own config/backups), with ansible-vault kept
only where a repo already uses it**. This needs zero daemons, no unseal
ceremony, and the "key" is a single 32-character age identity that fits on a
sheet of paper (or a YubiKey via `age-plugin-yubikey`) for disaster recovery.

Add **OpenBao in single-binary/single-node Raft mode** only if the stack later
needs *dynamic* secrets (short-lived DB creds, PKI issuance, transit signing)
or a real network API multiple services can call — not as the primary vault.
It is the only option here with genuine dynamic-secrets and revocation
semantics, at the cost of a seal/unseal lifecycle to automate.

Everything else surveyed (Infisical, Vaultwarden+bws, gopass, passage,
1Password Connect, systemd-creds) is either the wrong shape for
machine-to-machine IaC/CM secrets, adds a multi-container footprint out of
proportion to a homelab, or solves a narrower problem (local disk credential
injection, human password management) that sops/age or OpenBao already cover
for this project's needs.

---

## Landscape table

| Tool | Language / runtime | License | Footprint | Single binary? | Health (2025–2026) |
|---|---|---|---|---|---|
| **sops** | Go | MPL-2.0 (moved to CNCF Sandbox as `age-sops/sops`, [details](https://github.com/age-sops/sops)) | One static binary, no daemon | Yes | Active; CNCF sandbox project, ongoing releases |
| **age / age-plugin-yubikey** | Go | BSD/MIT-style | One static binary | Yes | Active; `filippo.io/age` is the reference impl, stable, minimal API |
| **ansible-vault** | Python (bundled in `ansible-core`) | GPL-3.0 | No extra install — ships in Ansible | N/A (built-in subcommand) | Stable/mature, unlikely to change; low feature velocity by design |
| **OpenBao** | Go | MPL-2.0, Linux Foundation governed | ~1 binary + Raft storage dir; comfortable in 1–2 GB RAM for single node ([source](https://docs.jacobpevans.com/security/tools/openbao)) | Yes (`bao` binary, integrated Raft storage removes external DB need) | Very active; v2.5.0 shipped Feb 2026 ([releases](https://github.com/openbao/openbao/releases)); forked from Vault 1.14 after HashiCorp's Aug 2023 BSL relicense ([duokey](https://duokey.com/en/resources/hashicorp-vault-bsl-license-change-openbao-migration)) |
| **HashiCorp Vault** | Go | **BUSL-1.1** (not OSI open source since 2023) | Similar to OpenBao | Yes | Actively developed but IBM-owned since the 2024 acquisition; license blocks "competing product" use cases — generally avoid for new homelab builds given OpenBao exists |
| **Infisical (self-hosted)** | TypeScript/Node + Go agent | MIT core, **proprietary `ee/`** enterprise tree gated by license key ([LICENSE](https://github.com/Infisical/infisical/blob/main/LICENSE)) | Server + **PostgreSQL + Redis**, ≥2 vCPU / 4 GB RAM recommended even for local eval ([docs](https://infisical.com/docs/self-hosting/deployment-options/docker-compose)) | No — 3-container Compose stack minimum | Active, well-funded; but architecturally heavier than everything else on this list |
| **gopass** | Go | MIT | One static binary + git-backed GPG/age store | Yes | Active; v1.16.x shipped Dec 2025, maintained by JustWatch |
| **pass** (passwordstore.org) | Shell + GPG | GPL-2.0/3.0 | Shell script, needs `gpg`, `git` | No (script, not compiled binary) | Low velocity, effectively feature-frozen but still works |
| **passage** | Go | MIT | Single binary, `pass`-compatible using age instead of GPG | Yes | Small, niche, low adoption/activity — treat as a curiosity, not infra |
| **Vaultwarden** | Rust | AGPL-3.0 | Single binary + SQLite, ~10 MB RAM idle, comfortable under 512 MB total ([instapods](https://instapods.com/blog/best-self-hosted-password-managers/)) | Yes (SQLite default; can also speak Postgres/MySQL) | Very active, widely deployed self-hosted Bitwarden-API-compatible server |
| **Bitwarden Secrets Manager CLI (`bws`)** | Rust CLI, talks to Bitwarden/Vaultwarden API | GPL-3.0 (CLI); server is separate | Just a CLI binary, but the *secrets manager* product is not what Vaultwarden's free OSS server implements out of the box | Yes (CLI) | Active; used in IaC pipelines to inject secrets as env vars |
| **systemd-creds** | C, part of systemd | LGPL-2.1/GPL-2.0 | Zero extra install on any systemd host; TPM2-backed | Yes (it's `systemd` itself) | Actively developed upstream; TPM2-sealed credential support is mainstream on modern distros |
| **1Password Connect** | Go server + sidecar model | Proprietary, requires 1Password subscription | 2-container deployment (`connect-api` + `connect-sync`) + a 1Password account | No | Actively maintained but SaaS-dependent and licensed — poor fit for an offline-capable homelab DR story |

---

## Per-option analysis

### sops + age — the default answer

**What it is:** sops encrypts *values* inside YAML/JSON/ENV/dotenv/binary
files while leaving keys in plaintext (so diffs stay readable and PRs/git
history stay meaningful); age is the modern key-management/backend it wraps
for the actual asymmetric encryption. ([SOPS guide](https://blog.gitguardian.com/a-comprehensive-guide-to-sops/), [homelab writeup](https://www.codedge.de/posts/managing-secrets-sops-homelab/))

- **Ansible integration:** the `community.sops` collection ships a lookup
  plugin (`community.sops.sops`) that decrypts inline during a playbook run;
  files with a `.sops.yaml`/`*.sops.yml` naming convention can be
  auto-decrypted by inventory plugins. No daemon, no server round-trip.
  ([Ansible community docs](https://docs.ansible.com/ansible/latest/collections/community/sops/docsite/guide.html))
- **Terraform/OpenTofu integration:** the community `carlpett/sops` provider
  (and equivalents) expose a `sops_file` data source that decrypts at plan
  time; OpenTofu 1.7+ also added **native state/plan encryption** with
  pluggable key providers (PBKDF2 passphrase, AWS/GCP/Azure KMS, and an
  OpenBao key provider) — sops/age is not yet a built-in key provider for
  *state* encryption specifically, but community projects like
  `josh/tofu-age-encryption` bridge that gap, and sops itself is commonly used
  to encrypt `.tfvars`/`terraform.tfstate.enc` wrapper files outside Tofu's
  native mechanism. ([OpenTofu 1.7 blog](https://opentofu.org/blog/opentofu-1-7-0/), [OpenTofu state encryption docs](https://opentofu.org/docs/language/state/encryption/), [tofu-age-encryption](https://github.com/josh/tofu-age-encryption))
- **At-rest model:** sops splits a random data key via **Shamir's Secret
  Sharing across configured key groups** (age recipients, PGP, or KMS ARNs) —
  you can require e.g. 2-of-3 recipients to decrypt, which doubles as a DR
  design: keep one age identity on the control host, one on a YubiKey, one on
  paper/offline. ([Shamir background](https://www.ledger.com/academy/topics/security/shamirs-secret-sharing))
- **Bootstrap/DR ergonomics:** best-in-class for this project. `age-keygen`
  produces a keypair in one line; the private key is a single line of text
  (`AGE-SECRET-KEY-1...`) that backs up trivially into a password manager, a
  printed QR code, or a hardware token via `age-plugin-yubikey`. No unseal
  ceremony, no running service to restore before secrets are readable — you
  only need the age identity file back.
- **Footprint:** effectively zero — one Go binary per tool, no daemon, no
  storage engine of its own (secrets live as encrypted blobs in the same git
  repo as the playbooks/IaC that use them).
- **License/health:** MPL-2.0, now living under the CNCF-adjacent
  `age-sops` org after Mozilla stepped back from active sops maintenance;
  age itself is a minimal, stable spec unlikely to need churn.
- **Gap:** sops/age is a *static* secret store — no leasing, no automatic
  rotation, no fine-grained per-identity ACL beyond "can this recipient
  decrypt this file." That's the right trade for a one-operator homelab, but
  it's why OpenBao stays on the list for anything needing issued/rotated
  credentials later.

### ansible-vault

- Ships inside `ansible-core`, so it costs nothing extra to have available.
  Encrypts *whole files* (or inline `!vault` strings) with a shared password
  or password-script (`--vault-password-file`), symmetric AES256.
- Weakness vs sops: to keep variable *names* visible in diffs (useful for
  code review) you must vault each value individually, which is tedious at
  scale — sops does this by default. ([comparison](https://tomsitcafe.com/2025/10/01/ansible-with-sops-secret-handling-on-ghost-level/))
- Still the right choice when a role/collection you're pulling in already
  expects `!vault` values, or for the single "master password" gate in
  front of an inventory — many homelabbers store *that one password* in
  Bitwarden/Vaultwarden and script its retrieval into
  `--vault-password-file`. ([Bitwarden+ansible-vault pattern](https://theorangeone.net/posts/ansible-vault-bitwarden/), [script example](https://github.com/guiand888/ansible-vault-bitwarden))
- **Recommendation:** keep it available as a fallback/interop path, but don't
  make it the primary mechanism — sops's per-value encryption and multi-key
  DR story are strictly better fits for this project's goals.

### OpenBao (Vault fork)

- **Why it exists:** HashiCorp relicensed Vault to BUSL-1.1 in August 2023;
  OpenBao forked the last MPL-2.0 commit (1.14.0) and is now a Linux
  Foundation project. API/CLI/Terraform-provider/Ansible-collection
  compatibility with Vault is preserved, so existing `hashicorp/vault`
  Terraform provider and `community.hashi_vault` Ansible collection largely
  work unmodified against a `bao` server. ([The New Stack](https://thenewstack.io/meet-openbao-an-open-source-fork-of-hashicorp-vault/), [duokey migration note](https://duokey.com/en/resources/hashicorp-vault-bsl-license-change-openbao-migration))
- **Single-binary/footprint:** the `bao` binary with **integrated Raft
  storage** needs no external database — genuinely single-binary for a
  single homelab node. 1–2 GB RAM is comfortable; documented single-node and
  small-Raft-cluster homelab deployments exist. ([homelab config example](https://github.com/cwage/homelab/blob/master/docs/openbao.md), [standalone install guide](https://blog.stderr.at/openshift-platform/security/secrets-management/openbao/2026-02-12-openbao-part-2-standalone-installation/))
- **Unseal ergonomics — the real cost:** Vault/OpenBao's Shamir seal requires
  a human to supply threshold key shares on every restart, which is exactly
  the kind of manual step this project's "trivially easy to bootstrap and
  recover" goal wants to avoid. The practical fix is **auto-unseal**: point
  OpenBao's `seal "transit"` (or a KMS-equivalent) at another always-on
  unseal key holder, or (homelab-friendly) OpenBao's own **static/environment-provided
  auto-unseal RFC**, which lets you unseal from a key baked into environment
  config rather than a second server. ([seal concepts](https://openbao.org/docs/concepts/seal/), [static auto-unseal RFC](https://openbao.org/community/rfcs/static-auto-unseal/), [transit auto-unseal walkthrough](https://labs.iximiuz.com/tutorials/openbao-vault-auto-unseal-transit-82d2a212), [migration between seal types](https://blog.stderr.at/openshift-platform/security/secrets-management/openbao/2026-03-05-openbao-part-6-auto-unsealing/))
  Either path is more moving parts than sops/age; treat it as the trade you
  accept only if you need OpenBao's actual differentiators.
- **Terraform integration:** OpenTofu 1.7+ ships a **built-in OpenBao key
  provider** for native state encryption — i.e., OpenBao can be the thing
  that hands OpenTofu its state-encryption key, not just a `vault_*`
  resource/data-source provider for secret values. ([OpenTofu 1.7 blog](https://opentofu.org/blog/opentofu-1-7-0/))
- **Ansible integration:** `community.hashi_vault` collection (lookup,
  connection, and module plugins) works against OpenBao's Vault-compatible
  API. A dedicated `openbao-vault` Ansible role also exists for
  install/config automation. ([role example](https://github.com/BenRedic-FyFazan/openbao-vault))
- **Verdict:** the right tool if/when the control stack needs dynamic
  secrets (leased DB creds, PKI, SSH CA, transit encryption-as-a-service) or
  a real secrets *API* multiple homelab services call at runtime. Overkill
  as the sole/primary store for "encrypt these Ansible vars and this
  Terraform state" — sops/age does that with none of the seal lifecycle.

### HashiCorp Vault

Same technical shape as OpenBao but **BUSL-1.1**, not OSI-approved open
source, and restricts "competing product or service" usage — and Vault is now
an IBM product post-acquisition. No reason to choose it over OpenBao for a
new homelab build in 2026; listed here only as the license/ancestry baseline.

### Infisical (self-hosted)

- MIT-licensed core, but the `ee/` tree (dynamic secrets, SCIM, LDAP,
  approval workflows, KMIP/HSM support) is proprietary and gated by a license
  key that phones home; the free tier is otherwise generous.
  ([LICENSE](https://github.com/Infisical/infisical/blob/main/LICENSE), [ToS](https://infisical.com/terms/self-hosted))
- **Footprint is the disqualifier for this project:** minimum viable
  self-host is Infisical server + **PostgreSQL + Redis**, ~2 vCPU/4 GB RAM
  recommended even for a proof-of-concept Compose stack. ([docs](https://infisical.com/docs/self-hosting/deployment-options/docker-compose))
  That's a heavier dependency graph than the *entire rest* of this project's
  control plane is aiming for, for a feature set (team dashboards, RBAC,
  approval flows) a one-operator homelab doesn't need.
- Has official Terraform provider and can be scripted into Ansible via its
  CLI/API, so integration isn't the problem — the multi-container footprint
  and the licensed-enterprise-tier UX (dynamic secrets, the thing that would
  make it worth the weight, sits behind the license key) are.
- **Verdict:** skip. Revisit only if the project ever grows into a
  multi-operator team tool, which is explicitly out of scope here.

### gopass

- Go, single static binary, git+GPG (or age) backed store, 100%
  `pass`-CLI-compatible with added multi-store "mounts." Actively maintained
  (JustWatch), v1.16.x in Dec 2025.
- No native Ansible/Terraform provider ecosystem to speak of — it's a
  human-facing CLI password manager, not designed as a machine secret
  backend for CI/CM tooling. You *can* shell out to it from a playbook the
  same way you'd shell out to `bw`/`bws`, but there's no first-class lookup
  plugin or Terraform data source.
- **Verdict:** good personal secret store to sit *alongside* the control
  stack (e.g., storing the age identity's backup passphrase, admin
  credentials), not a candidate to replace sops/age or OpenBao as the
  machine-facing secret backend.

### pass / passage

- `pass` is the GPG-based original; feature-frozen but functional, no
  compiled binary (needs `gpg`+`git`+POSIX shell), no IaC-tool integration.
- `passage` swaps GPG for age in the same pass-compatible design, but is a
  small/low-adoption project — a nice idea, largely superseded in practice
  by using sops directly with age recipients, which already has the
  Ansible/Terraform tooling this project needs.
- **Verdict:** skip both; sops+age covers the same ground with real IaC
  integrations.

### Vaultwarden + Bitwarden Secrets Manager CLI (`bws`)

- Vaultwarden itself (Rust, single binary + SQLite, ~10 MB RAM idle) is an
  excellent lightweight **Bitwarden-API-compatible password vault** for
  human logins/TOTP — genuinely single-binary and homelab-appropriate.
  ([footprint source](https://instapods.com/blog/best-self-hosted-password-managers/))
- However, Bitwarden's **Secrets Manager** product (the machine-secrets /
  `bws` CLI / Terraform-provider side that IaC pipelines actually integrate
  against) is a *separate* API surface from the plain password-vault API that
  Vaultwarden's OSS server implements — Vaultwarden does not (as of this
  writing) reimplement the Secrets Manager API, so `bws`/the
  `maxlaverse/bitwarden` Terraform provider target Bitwarden's own cloud (or
  the commercial self-hosted Bitwarden server), not vanilla Vaultwarden.
  ([terraform provider docs](https://registry.terraform.io/providers/maxlaverse/bitwarden/latest/docs), [bws CLI docs](https://bitwarden.com/help/secrets-manager-cli/))
- In practice, homelabbers use the *login-vault* half (Vaultwarden + `bw`
  CLI) to hold the **ansible-vault password** or the **age identity
  passphrase**, then have Ansible fetch it via a wrapper script at run time —
  a fine pattern for bootstrapping a human-operated key, but it's securing
  the *outer* key, not replacing sops/age/OpenBao as the secrets-at-rest
  layer for playbook variables or Terraform state. ([pattern write-up](https://blog.guillaumea.fr/post/ansible-vault-bitwarden-integration/), [IaC example](https://deepwiki.com/Mafyuh/iac/6-secrets-management))
- **Verdict:** worth running as the operator's personal/team password vault
  and as the anchor for retrieving the sops/age root key on a fresh
  bootstrap — not as the machine-secrets backend itself unless you also
  stand up genuine Bitwarden Secrets Manager (cloud or licensed
  self-hosted), which reintroduces cost/complexity this project wants to
  avoid.

### systemd-creds

- Zero-install on any systemd host: encrypts credentials at rest with
  AES-256-GCM, optionally sealed to the local **TPM2** chip (splitting the
  master key across TPM + on-disk file by default), decrypts only into a
  non-swappable tmpfs for the lifetime of the service. ([systemd.io](https://systemd.io/CREDENTIALS/), [smallstep writeup](https://smallstep.com/blog/systemd-creds-hardware-protected-secrets/))
- No Ansible module or Terraform provider exists for it — it's a *local
  delivery* mechanism for a systemd unit's own runtime secrets (e.g., a
  service's DB password baked into its `.service` file at deploy time), not
  a place to store/version secrets across hosts.
- **Verdict:** valuable as the **last-mile** step — e.g., after sops/age or
  OpenBao hands the control stack's own service (if containerized as a
  systemd unit) a plaintext secret at deploy time, wrap it with
  `systemd-creds encrypt` so it's TPM-sealed at rest on that specific host
  rather than sitting in a plaintext env file. Complementary, not
  competing.

### 1Password Connect

- Requires a paid 1Password account/subscription and runs as **two
  containers** (`connect-api` + `connect-sync`) plus a credentials file —
  not single-binary, not free, and its DR story depends on 1Password's own
  cloud remaining reachable/available (Connect syncs from the 1Password
  cloud vault). ([provider docs](https://developer.1password.com/docs/terraform/), [provider releases](https://github.com/1Password/terraform-provider-onepassword/releases))
- Has mature Terraform provider and an Ansible Connect collection, so
  integration quality is genuinely good — this is a "great enterprise
  choice, wrong shape for a self-contained homelab" case, not a "bad
  tool" case.
- **Verdict:** skip. Introduces an external paid SaaS dependency into a
  project whose explicit goal is disaster recovery that doesn't depend on
  any one vendor staying up.

---

## Integration notes vs. project goals

| Goal | sops+age | ansible-vault | OpenBao (single node) |
|---|---|---|---|
| Single-binary/lightweight | Yes — no daemon | Yes — built into Ansible | Yes, but is a long-running daemon with a seal state |
| Ansible integration | `community.sops` lookup plugin, inline decrypt | Native (`--vault-password-file`, `!vault`) | `community.hashi_vault` collection |
| Terraform/OpenTofu integration | Community `sops` provider (data source); community age state-encryption bridge | None (not IaC-oriented) | Official `vault` Terraform provider **and** a built-in OpenTofu 1.7+ key provider for native state encryption |
| Secret-at-rest model | Per-value AEAD, Shamir-split across recipients | Whole-file/whole-value AES256-CBC | Encrypted storage backend (Raft) behind a seal barrier |
| Bootstrap ergonomics | `age-keygen`, done | Set a vault password, done | Init + unseal ceremony (mitigated by auto-unseal, itself another thing to configure) |
| DR / key recovery | Multiple age recipients (host key + YubiKey + paper backup) via SSS threshold | Whatever you did with the vault password (no built-in multi-key story) | Shamir unseal keys (5 by default, 3 threshold) **or** recovery keys under auto-unseal — more ceremony to design and rehearse |
| Reactive/event-driven fit | Fine — a webhook handler can `sops -d` a file synchronously, no network dependency | Same | Better fit if a runbook needs to *fetch a freshly leased credential* rather than a static one |

**Backup-of-secrets angle:** the project also wants encrypted backups of its
own state/config/secrets to NAS/S3/GitHub. With sops+age, that's almost free
— the secrets are *already* encrypted blobs in the repo/config tree, so
"back up the repo" already satisfies "back up the secrets encryptedly." With
OpenBao, you additionally need to back up (and test-restore) the Raft
storage snapshot, which is a heavier, OpenBao-specific DR procedure
(`bao operator raft snapshot`).

---

## Recommendation

1. **Adopt sops + age as the primary secrets-at-rest mechanism** for both
   Ansible variables and Terraform/OpenTofu `.tfvars`. Store age recipients
   for: the control-stack host, a YubiKey (`age-plugin-yubikey`), and one
   offline/paper backup identity — configured as a sops key group so any one
   of them can decrypt independently (or require 2-of-3 if higher assurance
   is wanted).
2. **Keep ansible-vault available but secondary** — use it only where a
   third-party role/collection expects `!vault`-encrypted values; don't
   author new secrets in it.
3. **Defer OpenBao** until/unless the stack needs dynamic secrets (leased
   creds, PKI issuance) or a real secrets API for multiple services. When
   that need arrives, run it single-node with integrated Raft storage and
   **static/environment auto-unseal** (or transit auto-unseal against a
   second, always-on tiny node) so restart doesn't require a manual unseal —
   and make Raft snapshotting part of the backup routine from day one.
4. **Do not add Infisical or 1Password Connect** — footprint and
   vendor/subscription dependency respectively work against the "trivially
   recoverable, self-contained" goal.
5. **Optional nice-to-haves, layered on top rather than replacing the core:**
   - Vaultwarden, purely as the operator's personal password vault and as
     the place to stash the age identity's backup passphrase / recovery
     material — not as the machine-secrets backend.
   - `systemd-creds` for the last-mile TPM-sealing of a plaintext secret
     once it's been decrypted onto a specific host for a specific systemd
     unit.
   - gopass as a personal CLI convenience if the operator wants a
     `pass`-style interface to their own credentials; not part of the
     control-plane's automation path.

---

## Sources

- [community.sops Ansible collection guide](https://docs.ansible.com/ansible/latest/collections/community/sops/docsite/guide.html)
- [sops (age-sops org)](https://github.com/age-sops/sops)
- [age (age-sops org)](https://github.com/age-sops/age)
- [SOPS comprehensive guide — GitGuardian](https://blog.gitguardian.com/a-comprehensive-guide-to-sops/)
- [Managing secrets with SOPS in your homelab — codedge](https://www.codedge.de/posts/managing-secrets-sops-homelab/)
- [Ansible With SOPS — tomsitcafe](https://tomsitcafe.com/2025/10/01/ansible-with-sops-secret-handling-on-ghost-level/)
- [Storing Ansible Vault password in Bitwarden — TheOrangeOne](https://theorangeone.net/posts/ansible-vault-bitwarden/)
- [ansible-vault-bitwarden script](https://github.com/guiand888/ansible-vault-bitwarden)
- [Integrating Ansible Vault with Bitwarden — blog.guillaumea.fr](https://blog.guillaumea.fr/post/ansible-vault-bitwarden-integration/)
- [OpenTofu 1.7.0 release blog — state encryption](https://opentofu.org/blog/opentofu-1-7-0/)
- [OpenTofu state & plan encryption docs](https://opentofu.org/docs/language/state/encryption/)
- [tofu-age-encryption (community)](https://github.com/josh/tofu-age-encryption)
- [OpenBao releases](https://github.com/openbao/openbao/releases)
- [OpenBao — Jacob P Evans notes](https://docs.jacobpevans.com/security/tools/openbao)
- [OpenBao standalone install guide](https://blog.stderr.at/openshift-platform/security/secrets-management/openbao/2026-02-12-openbao-part-2-standalone-installation/)
- [OpenBao homelab config example](https://github.com/cwage/homelab/blob/master/docs/openbao.md)
- [Meet OpenBao — The New Stack](https://thenewstack.io/meet-openbao-an-open-source-fork-of-hashicorp-vault/)
- [HashiCorp Vault BSL license change / OpenBao migration — DuoKey](https://duokey.com/en/resources/hashicorp-vault-bsl-license-change-openbao-migration)
- [OpenBao seal/unseal concepts](https://openbao.org/docs/concepts/seal/)
- [OpenBao static auto-unseal RFC](https://openbao.org/community/rfcs/static-auto-unseal/)
- [OpenBao transit auto-unseal walkthrough — iximiuz Labs](https://labs.iximiuz.com/tutorials/openbao-vault-auto-unseal-transit-82d2a212)
- [OpenBao auto-unseal migration guide — blog.stderr.at](https://blog.stderr.at/openshift-platform/security/secrets-management/openbao/2026-03-05-openbao-part-6-auto-unsealing/)
- [openbao-vault Ansible role](https://github.com/BenRedic-FyFazan/openbao-vault)
- [Infisical LICENSE](https://github.com/Infisical/infisical/blob/main/LICENSE)
- [Infisical self-hosted ToS](https://infisical.com/terms/self-hosted)
- [Infisical Docker Compose deployment docs](https://infisical.com/docs/self-hosting/deployment-options/docker-compose)
- [gopass GitHub](https://github.com/gopasspw/gopass)
- [passage (age-based pass alternative)](https://github.com/chrisswanda/passage)
- [Vaultwarden footprint — instapods comparison](https://instapods.com/blog/best-self-hosted-password-managers/)
- [Vaultwarden container image wiki](https://github.com/dani-garcia/vaultwarden/wiki/Which-container-image-to-use)
- [Bitwarden Secrets Manager CLI docs](https://bitwarden.com/help/secrets-manager-cli/)
- [maxlaverse/bitwarden Terraform provider docs](https://registry.terraform.io/providers/maxlaverse/bitwarden/latest/docs)
- [IaC secrets management example using Bitwarden — DeepWiki](https://deepwiki.com/Mafyuh/iac/6-secrets-management)
- [systemd System and Service Credentials](https://systemd.io/CREDENTIALS/)
- [The magic of systemd-creds — Smallstep](https://smallstep.com/blog/systemd-creds-hardware-protected-secrets/)
- [1Password Terraform provider docs](https://developer.1password.com/docs/terraform/)
- [1Password Terraform provider releases](https://github.com/1Password/terraform-provider-onepassword/releases)
- [Shamir's Secret Sharing explainer — Ledger](https://www.ledger.com/academy/topics/security/shamirs-secret-sharing)
