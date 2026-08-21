# Delivery plan

The ordering rule is not "easiest first" or "most visible first": **the recovery chain is
right on day one and is never invalidated afterwards.** Everything later is additive.

| Milestone | Contents | Effort (FTE-wks) |
|---|---|---|
| **v0.1 — "provable core"** | Skeleton; secrets envelope including the fuzzed redaction filter; job queue and executor; Ansible executor (runner plus vault/sops wiring, [amendment 6](amendments.md#6-ansible-vault-is-designed-not-asserted)); backup engine, all three legs including the orphan-commit GitHub leg; restore, `install.sh`, and the CI-executed recovery card; a two-screen TUI | ~7.5 |
| **v0.5 — "both halves"** | OpenTofu executor, HTTP state backend, drift, plan-gated applies; scheduler; offline provider and collection caches; the full TUI (8 screens plus wizard); Wish/SSH serving | ~7 |
| **v1.0** | Event ingress, CEL rules, guardrails; approvals and ntfy transport; the scoped notification and escalation surface; web UI; `/metrics` plus seeded alerts; host onboarding; upgrade and rollback; packaging with budgets enforced in CI; docs including threat model and adoption runbooks | ~8.5 |
| Deferred | Wails desktop, OpenBao resolver, Litestream embedding, GoAlert-class escalation | — |

**Total ≈ 23 FTE-weeks** (~3 focused months). Every milestone is independently useful:
v0.1 alone is "run Ansible playbooks with encrypted secrets, back up to three places, prove
the restore."

The estimate is realistic rather than optimistic precisely because roughly 70% of the hard
work — the backup engine, the IaC executor, the encryption, the rules matching — is
delegated to restic, OpenTofu, age, and cel-go rather than written.

## Risks carried into delivery

| Risk | Mitigation, designed rather than promised |
|---|---|
| **Solo-maintainer bus factor** — the dominant risk and the honest cost of this architecture | Nothing in a bespoke format; `exocomp export --portable` emitting a Semaphore-importable structure; the manual recovery path executed in CI |
| **Crypto envelope subtly wrong** — a nonce reuse or a secret leaked into `job_event` turns the vault into theatre | Zero novel cryptography (`filippo.io/age`, stdlib GCM and HKDF); fuzzed redactor plus a belt-and-braces scan against the known-secret set; a canary secret in every drill; external review of `internal/secrets` (~600 lines) before v1.0 |
| **Subprocess contract drift** — an upstream `--json` change breaks parsing mid-restore | Pinned versions recorded in `MANIFEST.json`; golden-file corpora regenerated on every version bump; success decided by exit code only, so a schema change degrades the display, not correctness |
| **The reactive loop causes the outage it was meant to prevent** | Guardrails in the queue with SQLite ledgers that survive a crash-loop; caps disable rules rather than throttling them; kill switch one keystroke from every screen; every event persisted with its outcome |
| **Scope creep into a bad AWX** | The anti-scope contract in [requirements](requirements.md#anti-scope), with the idle-RSS budget enforced by CI |
