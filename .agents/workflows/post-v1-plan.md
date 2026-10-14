# Post-v1.0 program (operator-directed 2026-08-22)

Trigger: begins only after the acceptance bar is verifiably met (M3d gate green +
maintenance). Met 2026-08-22; the release label was later withdrawn in favour of
the pre-1.0 line (Decision 0022), which leaves the bar itself unchanged.

## Feature track (sequential milestones, each build+maintain workflow pair)

1. **Litestream** — continuous SQLite WAL replication as an optional sidecar for
   seconds-level RPO on exocomp.db (candidate-thin-core §7.4 deferred it; a record when
   built). Reconcile with the existing 3-leg restic backup: Litestream is the
   fast-RPO complement, restic stays the authoritative encrypted DR chain. Note:
   Litestream does not encrypt — front with an encrypted target or LAN-local only,
   consistent with the earlier hybrid-candidate finding.
   [Operator wrote "lightstream" — read as Litestream unless corrected.]
2. **GoAlert** — escalation beyond v1.0's ntfy-priority + dead-man: on-call
   schedules, ack-tracking, escalation chains. Integrate as an external notify
   target via the notify-layer seam (do NOT absorb into the daemon — anti-scope:
   no second heavy service inside the container; GoAlert runs beside it, exocomp
   speaks to it). Reconcile with docs/design/user-feedback.md (Priya's
   second-transport-on-unacked + synthetic-page asks).
3. **OpenBao** — dynamic/leased secrets via the SecretResolver seam Decision 0003 left
   open (bao:// references); the static age envelope stays the default and the
   root of trust. External KMS/Transit as a root-of-trust option (Decision 0003.10
   adoption path). Keep seal/unseal off the bootstrap critical path.

## Continuous track (parallel, at Claude's discretion, recurring)

- **Security red team**: adversarial subagents attacking the running system — the
  envelope (evil-maid, keyring rollback, target-side forgery), the daemon trust
  boundary, the reactive/approval path (token replay, gate bypass), supply chain.
  Attempts reproduce against real binaries; findings ranked by real single-operator
  risk. Feeds a threat-model doc and fix backlog.
- **Security blue team**: hardening + detection response to red-team findings —
  fixes, seeded self-alerts, the deferred supply-chain items (cosign, SLSA
  provenance, reproducible builds, signed installer, SBOM attestation), the audit
  hash chain (Decision 0007 revival), TPM PCR-policy + mlock bounds statements.
- **User persona reviews**: re-run the four personas (Marcus/Priya/Tom/Elena) plus
  new ones against each shipped milestone; convergences become backlog, divergences
  become recorded segment trade-offs. Same adversarial framing as the M-series.

These run as their own workflows; not gated behind the feature track.
