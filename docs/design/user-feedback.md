# Persona review — findings and disposition

Date: 2026-08-21. Four target-user personas evaluated the shipped M1/M2 feature set
and the committed v1.0 scope, briefed to be adversarial adopters. Full transcripts are
session artifacts; this page keeps what changes decisions.

Personas: **Marcus** (veteran homelabber, 200-task Ansible repo, AWX refugee) ·
**Priya** (staff SRE, Terraform-Cloud/Vault reflexes, refuses enterprise ops at home) ·
**Tom** (time-poor generalist, bash + compose, no Ansible/Tofu) · **Elena** (security
engineer, SELinux enforcing, reads ADRs and licenses).

## Convergent findings (2+ personas independently)

1. **The DR card must derive every password, not just restic's.** If vault passwords
   are HKDF subkeys only `exocompd` derives, a dead daemon bricks the operator's own
   playbooks (Marcus); after rotation, an old snapshot must open with the current card
   — key generations need drill coverage (Priya).
2. **"exocomp backs up its own state, not your data" must be stated in bold** (Marcus),
   and backing up *operator* data (compose volumes, service configs) is the adoption
   unlock for the generalist segment (Tom).
3. **Ansible needs a plan-equivalent**: scheduled `--check --diff` with changed-count
   as drift (Marcus) / check-mode as a gated pseudo-plan in the approval flow (Priya).
4. **Reboots and maintenance must not page or trigger**: silences with TTL, phone-set,
   loudly visible while active (Priya hard blocker; Tom's ignore-for-a-month framing).
5. **Anchor integrity off-host**: HMAC the backup manifest + version the keyring,
   refuse downgrades (Elena — closes the one silent-unrecoverability gap); audit hash
   chain only worth building with its head exported to the git leg (Priya, Elena
   agreeing from opposite directions).

## Per-persona headlines

- **Marcus** — would replace the cron box + ~60% of his Makefile; will not migrate
  until: inventory is designed (currently "a ghost" — goal named, no page, no dynamic
  -inventory token binding), ad-hoc runs exist, vault derivation is on the card, and
  the project has commit history that isn't one day old. Rates the exit design
  (no bespoke formats) as better than AWX ever offered. Wants a JSON stdout callback
  instead of line-scanned playbook output.
- **Priya** — adopt-with-conditions; the saved-plan/linked-jobs approval model is
  better than TFC's on the property she cares about. Conditions: silences (blocker),
  key-generation drill (blocker), CEL policy over plan JSON before the gate,
  structured destroys-first diff on the phone with sensitive values suppressed, git
  provenance with refuse-apply-if-tree-moved, second-transport escalation + periodic
  synthetic page, rule explain/replay. Flags plan artifacts + approval rendering as
  the least-protected place a secret appears.
- **Tom** — installs only if week one is "wizard → backups green → card in the fire
  safe" with everything else dark; wrap-my-script (shell runbooks as first-class
  on-ramp) and compose awareness are what make the tool *for him*; wizard screen 1
  must have a Recommended default instead of a TPM/YubiKey decision; NAS-only must be
  a legitimate wizard finish with a standing nag; the dead-man's switch is the
  feature — "a monitoring tool whose failure mode is silence launders my anxiety."
- **Elena** — conditional-leaning-yes; respects the CI-executed card, assert-don't-
  assume probes, exit-code trust boundary, and stated threat honesty. New findings:
  keyring rollback (no version/MAC), chunk parts-manifest outside the age envelope,
  unbounded mlock claims under rootless podman, unnamed TPM PCR policy. Conditions:
  signed releases + SBOM attestation + reproducible builds, in-repo signed installer
  with its hash on the card, manifest MAC + keyring versioning, the threat model
  shipped WITH v1.0, a written no-egress commitment. Process flag: the hybrid
  candidate's audit hash chain was dropped with no ADR recording the decision.

## Disposition

**Folded into M3 (v1.0) scope** — cheap, high-leverage, no new processes:
- Card prints the generalized derivation (any `info` string, vault-ids named) + test.
- Keyring version counter + HKDF-derived HMAC over manifest and parts-manifest;
  restore refuses downgrades. (One subkey, existing machinery.)
- Silences / maintenance windows in the reactive layer; resolve-cancels-approval.
- CEL policy pass over `tofu show -json` before the approval gate (seeded rules:
  destroy-count, protected addresses).
- Structured plan diff for approvals: counts, per-resource grouping, destroys first,
  sensitive attributes suppressed; plan artifacts encrypted at rest.
- Shell runbooks surfaced as a first-class on-ramp (wizard + docs), wizard screen-1
  Recommended default, NAS-only finish allowed with standing nag, dead-man mandatory.
- Ansible check-mode runbook flag with changed-count surfaced (drift analog).
- Inventory: document the repo-file model + secret binding for dynamic-inventory
  tokens (full inventory management: v1.1, ADR first).
- Docs: bold own-state-only backup statement; no-egress commitment; PCR-policy and
  mlock-bounds statements in the threat model (already a v1.0 deliverable).
- ADR 0007 recording the audit-chain deferral and its revival condition (head
  anchored off-host in the git leg).

**v1.1 backlog (ranked)**: operator-data backup sources incl. compose volumes;
full inventory management; ad-hoc command runs; JSON task-tree callback plugin;
sops-repo adoption/rekey runbook; restic-repo adoption semantics + retention spec;
second-transport escalation + synthetic page; rule explain/replay CLI; audit hash
chain with exported head; key-generation restore drill; supply chain (cosign, SLSA
provenance, CycloneDX attestation, reproducible builds, signed in-repo installer);
age-plugin-yubikey with its card impact stated; secrets-age alerting.

**Explicitly declined** (consistent with the anti-scope contract): on-call rotations,
dynamic/leased credentials, PR-comment integrations, multi-user RBAC.
