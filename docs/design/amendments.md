---
type: Design
title: "Amendments"
description: "The twelve normative changes that override the three candidate designs wherever they differ."
status: stable
tags: [design, decisions]
---

# Amendments

Twelve **normative** design decisions that override the three candidate designs wherever
they differ. They cluster at the
*edges* — credentials to reach a backup, retention that reclaims space, upgrade rollback,
host onboarding — which is where homelab tooling actually fails.

## 1. The recovery kit closes the credential loop

The kit must carry the credentials needed to *reach* the backup: restic cannot open an S3
repository without an access key, and that key would otherwise sit inside the backup.

The kit carries a **dedicated read-only storage credential per off-host leg** (S3 key
scoped to GET/LIST; GitHub fine-grained PAT with read-only contents), the healthchecks ping
URL, and the ansible-pull trust anchor fingerprint if pull mode is enabled. These are
`secret` rows tagged `on_recovery_kit=true`; the renderer refuses to render without them,
and rotating one regenerates the kit with a nag until reprinted. The **monthly DR drill
runs from a scratch container holding only the kit contents** — no host state, no shell
history — so this class of circularity fails a scheduled test instead of a real disaster.
See [Decision 0003](../decisions/0003-secrets-envelope.md).

## 2. A GitHub backup leg that actually reclaims space

Git keeps every blob reachable forever, so `git rm` on retention never shrinks the repo and
eventually hits GitHub's limits. The leg therefore pushes each snapshot as a **single
orphan commit, force-pushed** to a rolling window of refs (`backup/latest`,
`backup/weekly-N`), so old objects become unreachable and are reclaimed; **chunks archives
at 90 MB** with a manifest, because GitHub hard-rejects files at 100 MB; and checks repo
size via the API before pushing, **failing the leg loudly at 4 GB** rather than degrading
silently. This is within acceptable use for private repos at control-plane data volumes
(megabytes, not media libraries); the S3 leg is recommended as primary off-site regardless.

## 3. Upgrade and rollback of the control plane itself

- Every upgrade takes a **mandatory automatic pre-upgrade snapshot** (full, all legs,
  tagged `pre-upgrade-<version>`); the upgrade job refuses to proceed if it fails.
- `exocomp rollback` restores that snapshot and pins the previous image.
- DB migrations stay forward-only, so **rollback = restore**, stated explicitly. The
  pre-upgrade snapshot is what makes that safe.
- A scheduled **upgrade rehearsal** mirrors the restore drill: restore last night's
  snapshot into a scratch instance, run the new image's migrations, assert, discard.
- Self-update is opt-in and approval-gated. Recovery never depends on the thing being
  upgraded.

## 4. A whole-container resource budget

Target hardware is named: a **4 GB N100 or Raspberry Pi 4**, amd64 and arm64. Image ≤ 800
MB, `exocompd` idle RSS ≤ 60 MB (CI fails above 80), Quadlet `MemoryMax=1.5G`, **2
concurrent jobs by default** derived from memory (ansible forks and tofu providers are the
real consumers) with a documented sizing table. Provider and collection caches live in the
data volume behind a free-space guard; `job_event` is retained 90 days with full logs
gzipped into artifact storage. CI measures image size, idle RSS, and peak RSS of a
reference Ansible + OpenTofu job. Numbers in
[the bill of materials](../sbom/bill-of-materials.md).

## 5. Escalation, scoped honestly

v1 **is not PagerDuty** and says so. It ships ntfy priorities with re-publish at `urgent` at
50% of an approval or alert TTL, acknowledgement tracking on notifications that request it
(unacked → re-notify per policy), and the dead-man's switch. **healthchecks is a
first-class bootstrap step**: provisioned by the wizard, its ping URL stored, backed up,
and printed on the kit — it stops being an unmanaged external dependency. A `notifier`
interface seam is reserved for GoAlert-class escalation post-v1; on-call rotations are out
of scope.

## 6. ansible-vault is designed, not asserted

ansible-vault is the one secrets mechanism the operator most likely already has, so the
migration path is a section rather than a footnote.

- `project.vault_ids` maps **multiple `--vault-id` labels** to key-store entries
  (`dev@…`, `prod@…`), each either imported or HKDF-derived.
- The wizard's first-project screen has an **import-existing-vault-password** flow.
- A seeded **`vault-rekey` runbook** automates `ansible-vault rekey` across a repo for a
  chosen vault-id, approval-gated, with a pre-run git-bundle snapshot.
- `become_pass` and `ansible_ssh_pass` in `host_vars` are sourced via the `community.sops`
  lookup or vault files, never plaintext in the repo; the doctor scans for plaintext
  credentials in `host_vars` and warns.
- **Ownership rule**: sops+age owns new secrets; ansible-vault is the compatibility path
  for pre-existing content. Both feed from the same DEK root.

## 7. The printed card is tested code

A DR card whose commands do not run is worse than no card, and the failure modes are subtle
and invisible on paper — a pipeline stage that reads nothing, a KDF invocation that does not
consume stdin, mixed option spellings.

Normative rule: **the exact rendered text of the recovery card is executed in CI** on a
clean image against a fixture backup, and the derived restic password is diffed against
`exocompd`'s own derivation. The card is a build artifact, not documentation.

## 8. Offline-restorable runtime dependencies

DR must not depend on registry.opentofu.org or Ansible Galaxy being reachable.
`TF_PLUGIN_CACHE_DIR` plus a filesystem provider mirror and vendored Galaxy collections —
including `community.sops`, which the design itself depends on — live in the data volume
and are part of every backup. The restore drill asserts a play and a plan run **with
network egress disabled**. Detail in [execution](../architecture/execution.md).

## 9. Remote access and approval transport

The approval UX depends on a phone tapping an ntfy action button, which needs an endpoint
reachable from the phone — so the transport is specified rather than left as "put Tailscale
or Caddy in front". Defaults: unix socket locally; **ntfy self-hosted or
token-authenticated**, never a public topic, because approval URLs are bearer capabilities.
The supported remote pattern is **Tailscale/WireGuard-first** with a Caddy + ACME fallback
recipe. See [the platform contract](../architecture/platform-contract.md).

## 10. Root-of-trust seam and adoption paths

The envelope's key-provider is an interface: the local age identity is the default, and an
**external KMS or OpenBao Transit key can substitute at bootstrap** — which lines up with
OpenTofu's own support for Transit as a state-encryption key provider.

Explicit, reversible **adoption runbooks**, tested against fixtures: take over existing
OpenTofu state (`tofu init -migrate-state` with a documented back-out while the old backend
still exists), import an existing vault password, and register an existing restic
repository by verify-and-adopt rather than re-init.

## 11. Operational hardening

- **Host onboarding** is a seeded, first-class runbook: push SSH key, create
  `ansible_user`, sudoers, record host key. `known_hosts` lives in the DB and is backed up,
  so DR restores host trust. The connection matrix is documented (Linux over SSH; network
  devices via `network_cli`; jump hosts via `ProxyJump`; non-SSH appliances out of scope for
  v1). Per-host credentials are `secret` rows bound per inventory group, not one global key.
- **Mount and disk guards**: before any NAS leg, assert mountpoint, canary file, and free
  space — an unmounted mount path must fail, not silently fill the local disk with a second
  copy of the backup. A global free-space guard refuses new jobs and alerts before the
  volume-full cliff. NAS/SMB mount credentials become managed config (systemd mount units
  generated from the DB) and therefore live inside the backup.
- **Self-observability**: `/metrics` plus a seeded alert-rules file covering reactive
  engine paused, approvals pending past half their TTL, backup leg stale beyond 48 h, drill
  failed, state lock held stale, disk low, and job queue stuck. The control plane is
  monitored by the same stack it reacts to; "the automation stopped automating" is itself an
  alert.
- **Boot-time reconciliation** of interrupted jobs and stale locks, asserted in the restore
  drill — every restored snapshot contains such rows by construction.
- **Token lifecycle**: API tokens get their own table with scope, expiry, `last_used_at`
  and revocation, and every audit row records the resolved operator identity along with
  whether the action arrived via TUI, web, token, or approval callback.
- **Honest security claims**: state ciphertext is opaque *to backup targets*, not to the
  control plane, which derives the passphrase. The threat model states plainly that
  `exocompd` can decrypt everything it manages; the envelope protects backups, stolen
  disks, and off-host attackers — not root on the box.

## 12. ansible-pull, if enabled, stays inside the guardrails

Fleet-wide `ansible-pull` as an optional self-healing mode must not become a guardrail
bypass. Pull targets pin to a **tag the control plane advances only after a successful gated
run** — never a moving branch — commits and tags must be **signed**, and the trust anchor is
distributed at host onboarding and printed on the recovery kit.
