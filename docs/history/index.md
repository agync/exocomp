# History

Thirteen documents from the design process that produced this project, imported
**byte-identical** from the pre-repository design archive. They are provenance records, not
living specs.

## Read these as records

- **They mostly use the working name "control"** — daemon `controld`, database
  `control.db`, CLI `control`, domain `get.control.sh`. The project was renamed to
  **exocomp** after a collision check ([ADR 0002](../adr/0002-project-name-exocomp.md)).
  Twelve records predate the rename; the post-decision addendum was written after it and
  mixes both names (its nono section already says `exocomp.db` and `exocomp doctor`). Every occurrence of the old name maps mechanically: `controld` →
  `exocompd`, `control.db` → `exocomp.db`, `control <cmd>` → `exocomp <cmd>`.
  They are not rewritten, because rewriting a record is how a record stops being one.
- **Two of them were rejected.** `candidate-compose-first.md` and `candidate-hybrid.md`
  describe architectures that were *not* built. Nothing in them is normative.
- **The accepted candidate is superseded in detail.** `candidate-thin-core.md` is the
  design that was adopted, but the twelve
  [amendments](../design/amendments.md) override it wherever they differ — including its
  broken manual-recovery one-liner, its public-topic ntfy default, and its recovery kit
  that omitted the credentials needed to reach the backup.
- **Where a curated page and a history page disagree, the curated page wins.** These
  documents were not edited to keep them consistent with later decisions.

No absolute filesystem paths, home directories, or local file URLs appear in any imported
file; this was verified before import, so no substitutions were needed and every file is a
byte-for-byte copy of the archive original. The one relative cross-reference in the set
(`candidate-hybrid.md` → `../research/prior-art.md`) still resolves, because the
`design/` and `research/` split is preserved here.

## Candidate designs and critique

| Document | What it is |
|---|---|
| [candidate-thin-core.md](design/candidate-thin-core.md) | The accepted architecture: one Go binary owning state, sequencing, keys and recovery. Full component tables, data model, and diagrams — the detail the curated pages summarise |
| [candidate-compose-first.md](design/candidate-compose-first.md) | Rejected: Semaphore UI as the execution engine with a `ctl` companion for glue |
| [candidate-hybrid.md](design/candidate-hybrid.md) | Rejected: Semaphore UI plus a `keel` supervisor owning keys and backup |
| [critique.md](design/critique.md) | The adversarial completeness review of all three, 30 enumerated gaps (sections A–H) across DR, backup mechanics, bypassable guardrails, uncovered requirements, auth and remote access, resource budget and upgrade, host lifecycle, and self-observability — plus seven claims flagged as unverified |

## Research notes

Eight landscape surveys, each written before any architecture existed. Every one landed on
the same shape: the components are mature and boring, and the glue is what nobody ships.

| Note | Verdict |
|---|---|
| [ansible-executors.md](research/ansible-executors.md) | AWX is frozen (no release since July 2024, needs Kubernetes and 16 GB); Rundeck is a JVM with webhooks paywalled; Semaphore UI is the standout single binary but paywalls Terraform state |
| [terraform-orchestration.md](research/terraform-orchestration.md) | OpenTofu (MPL) over Terraform (BSL), with native state encryption since 1.7. No TACoS platform fits homelab scale, and MinIO breaks the S3 `use_lockfile` conditional write |
| [secrets-management.md](research/secrets-management.md) | sops + age wins — no daemon, no unseal ceremony, a root key that fits on a printed page. OpenBao is genuinely single-binary but its seal/unseal lifecycle fights easy recovery |
| [backup-primitives.md](research/backup-primitives.md) | restic, shelled out — its no-Go-library stance is deliberate and permanent, so the subprocess *is* the designed integration. Litestream is optional for seconds-level RPO |
| [event-driven-automation.md](research/event-driven-automation.md) | Nothing fits. StackStorm is heavy, Grafana OnCall was archived, ansible-rulebook costs ~250 MB plus Drools. The answer is thin glue: webhooks → CEL rules → guardrails |
| [ui-stack.md](research/ui-stack.md) | Go end to end — Bubble Tea v2 served over SSH via Wish, templ + htmx embedded for web, Wails for desktop. Textual has no static-binary story; ratatui forces a second language |
| [prior-art.md](research/prior-art.md) | No existing project covers the brief. Semaphore is the closest half; Coolify, Portainer and Dokploy solve app deployment, not infra runs. Compose, do not adopt a platform |
| [bootstrap-recovery.md](research/bootstrap-recovery.md) | The PocketBase shape: one binary, embedded SQLite, an age emergency kit modelled on Bitwarden's, Quadlet over compose, and a separately versioned `install.sh` that never depends on the thing that broke |

## Post-decision addendum

| Note | Verdict |
|---|---|
| [addendum-turso-pglite-ghostty.md](research/addendum-turso-pglite-ghostty.md) | Four late challengers evaluated *after* the design was accepted. Turso: discard, defer-watch on the Rust rewrite. PGlite: discard — no CGO-free Go embedding exists. Ghostty/libghostty: defer-watch, no integration. **nono: adopted**, and it became the `local-sandboxed` executor tier ([ADR 0005](../adr/0005-job-sandboxing-nono.md)) |
