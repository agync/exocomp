# Knowledge base evolution log

Structural history of this bundle: pages added, deprecated, moved, merged, or split, and
corrections of fact. **This is not a software changelog** — releases, features, and bug
fixes belong to the project's release notes, not here.

Newest first.

## 2026-08-21 — M2b caches: offline-caches page added, MinIO assessment closed out

[operations/offline-caches.md](operations/offline-caches.md) is new: `playbook vendor`
and the ansible collections cache, the tofu plugin cache and its `OfflineProviders`
mode, and how to verify offline resolution for real. Linked from
`operations/index.md` and the top-level `index.md`.

[operations/disaster-recovery.md](operations/disaster-recovery.md) gained the operator
commands for sealing a `restic_s3` target's S3 credentials
(`s3/<target>/access-key-id`/`secret-access-key`) — the row naming and fallback
behavior were already documented, but not how an operator actually seals one; the
backup lane shipped the mechanism without that step.

[sbom/minio-assessment.md](sbom/minio-assessment.md)'s decision section is no longer a
backlog item: the S3 test fixture (`internal/backup/s3_test.go`,
`test/e2e/m1-gate.sh`) is SeaweedFS, verified against the real restic leg and this
project's own SigV4 client, with the full e2e gate passing against it. Two
fixture-only quirks the swap surfaced are recorded there: SeaweedFS rejects a signed
request without a real `-s3.config` identity (anonymous mode is not viable for
restic), and the mounted config needs world-readable permissions because rootless
podman's UID remap does not preserve ownership into the container.

## 2026-08-21 — M2a scheduler closes half the M1 backup carve-out

`architecture/overview.md`'s "one queue, no side doors" section named `exocomp backup run`
as a direct CLI invocation outside the queue, "because there is no scheduler yet to enqueue
[it] against." `internal/sched` now exists: it fires cron-scheduled jobs through
`(*queue.Queue).Enqueue`, the same path everything else uses, so the reason the carve-out
gave no longer holds for backups. The paragraph now says what `backup run` and
`schedule add --kind backup` actually do — one queued job per target, concurrency key
`backup:<target>` — and narrows to `exocomp restore`, which stays a direct invocation:
restoring is an operator action at a cold start, not something the scheduler reaches.
## 2026-08-21 — M2a hardening lands git chunking and sealed S3 credentials

`operations/disaster-recovery.md`'s leg table previously flagged the git leg's 90 MB
chunking and 4 GB repo-size guard as not-yet-shipped M2 work (see the M1 gate corrections
entry below). Both now ship in `internal/backup/chunk.go`, so the row is corrected to
describe the shipped mechanism instead of its absence. The S3 leg row gains the sealed
credential resolution added alongside it: `s3/<target>/access-key-id` and
`.../secret-access-key` rows, process-environment fallback with a logged warning. A new
line documents `exocomp ssh-serve`, which was not previously mentioned anywhere in the
bundle.
## 2026-08-21 — M2a tofu corrections

Building the OpenTofu half found three claims in
[ADR 0004](adr/0004-opentofu-http-state-backend.md) the shipped code does not
support (code wins, per [AGENTS.md](../AGENTS.md)):

- **Bearer scope.** The ADR said the backend bearer is "valid only for that job's
  lifetime". It is derived per workspace and is therefore stable. A per-job token
  cannot work as written: a saved plan file carries the bearer it was made against
  and is applied by a later job.
- **Drift representation.** The ADR said exit 2 sets `job.drift = true`. No such
  column exists, and `executor.Result` carries one exit code that the queue reads
  as the job's verdict, so the driver records the plan outcome in the job's
  append-only `job_event` log and reports exit 0. The plan job succeeds.
- **Retention pruning** of old `tfstate` serials was stated as a property. Every
  serial is kept; nothing prunes them yet, and the page now says so.

Added to the same ADR's consequences: a saved plan file is a credential, because it
carries the backend address and bearer, which is also why the backend address has to
survive between a plan and the apply that consumes it.

`architecture/execution.md` gained an "OpenTofu runs" section describing the shipped
driver: the generated backend override, the private `TF_DATA_DIR`, the `TF_ENCRYPTION`
passphrase, the environment allowlist, and why two runs cannot share a workspace
directory.

Two rows of `operations/disaster-recovery.md` said `exocompd serve` "is not yet
implemented", which the M1 corrections had made true. `exocomp serve` now exists and
serves the state backend; the startup integrity gating, the single-recipient nag, and
the bootstrap wizard those rows point at are still unbuilt, so the rows name that
instead of the command.

## 2026-08-21 — Bundle created

The `docs/` bundle was created as an OKF v0.2 knowledge base, curated from the
pre-repository design archive that produced the project. Nothing existed before this entry.

**Added — sections and pages**

| Path | Contents |
|---|---|
| [index.md](index.md) | Root index and section map |
| [log.md](log.md) | This log |
| [adr/](adr/index.md) | Index plus ADRs 0001–0006, all Accepted |
| [architecture/](architecture/index.md) | Index, `overview.md`, `platform-contract.md`, `execution.md` |
| [design/](design/index.md) | Index, `requirements.md`, `amendments.md`, `delivery.md` |
| [operations/](operations/index.md) | Index, `bootstrap.md`, `disaster-recovery.md` |
| [sbom/](sbom/index.md) | Index, `bill-of-materials.md` |
| [history/](history/index.md) | Index plus 13 provenance records imported byte-identical |

**Sources.** The authoritative source was the archive's accepted technical design document.
Supporting sources were eight research notes, a post-decision addendum, three candidate
designs, and a 30-gap adversarial critique. The curated pages are summaries; the archive
documents themselves were imported unchanged into `history/`.

**Naming.** Most of the archive predates the rename from the working name "control" to
**exocomp** ([ADR 0002](adr/0002-project-name-exocomp.md)); the post-decision addendum was
written after it and mixes both names. Curated pages use the current name throughout.
Imported history pages were deliberately **not** rewritten — the reasoning is stated in
[history/index.md](history/index.md).

**Import fidelity.** All 13 history files are byte-identical to the archive originals. They
were checked for absolute filesystem paths, home directories, and local file URLs before
import; none were present, so no substitutions were needed. The archive's `design/` and
`research/` split is preserved under `history/` so the one relative cross-reference in the
set still resolves.

**Not imported.** The archive's rendered HTML dossier, which is a presentation of the same
material rather than a source, and the accepted design document itself, whose content is
distributed across the curated pages rather than duplicated.

**Precedence established.** Where a curated page and a history page disagree, the curated
page wins — history documents were not edited to stay consistent with later decisions. The
twelve [amendments](design/amendments.md) override the accepted candidate design wherever
they differ.

## 2026-08-21 — M0 maintenance corrections

Adversarial review of the integrated bundle corrected these facts (code and the
critique source win over curated prose):

- The critique's gap count is **30 enumerated gaps across sections A–H**, not 22;
  the wrong count appeared on seven pages and traced to trusting a summary of the
  review rather than the review itself. All seven corrected.
- The naming claim over `history/` was over-broad: the post-decision addendum was
  written *after* the rename and mixes both names. `history/index.md`, `index.md`,
  and this log now say so.
- `platform-contract.md` now states its CI enforcement is designed, not yet built.
- `install.sh` gained the documented `--restore` / `--rollback --image-tag` forms,
  resolving the contradiction with `operations/disaster-recovery.md`,
  `operations/bootstrap.md`, and `design/amendments.md` in code's favor by making
  the docs true.
- Effort figures: ADR 0001's comparison row now carries its units caveat;
  `delivery.md` explains the ~22 vs ~23 FTE-week split.

## 2026-08-21 — M1a implementation landed

The secrets envelope and the store/queue core now exist in code; pages describing
them are design-accurate but the implementation is authoritative:

- The recovery card's derivation block is executable text enforced by test
  (`internal/secrets/kit_test.go` runs the card's age + openssl lines verbatim and
  the derived password opens a real restic repository). The card uses `od`/`tr`
  instead of `xxd` for bare-host portability.
- The HKDF domain-separation salt is live: `exocomp/v1`
  (see [ADR 0003](adr/0003-secrets-envelope.md); the archive's `control/v1` was
  never used by any artifact).
- The job state machine, append-only `job_event` (trigger-enforced), per-key
  serialization, idempotency dedup, and the interrupted-job boot sweep are
  implemented and race-tested as designed in
  [architecture/execution.md](architecture/execution.md).

## 2026-08-21 — M1b implementation landed

- Ansible driver (`internal/executor/ansible.go`): env allowlist, per-job 0700
  private dir for vault-password and extra-vars files, `--vault-id` from the
  envelope-derived password; queue events pass through the redactor before
  persistence (`internal/queue/redact.go`). `exocomp playbook run` is live.
- Backup engine (`internal/backup`): three legs implemented and tested against
  real restic, a real bare git repo (single rolling orphan commit verified), and
  MinIO-under-podman for S3; `keyring.age` + `RECOVERY.md` land at every target
  root outside the restic repo; `exocomp backup run` / `exocomp restore` live.
- Correction: [operations/disaster-recovery.md](operations/disaster-recovery.md)
  staging tree updated to the implemented M1 contents (snapshot.db, keyring.age,
  MANIFEST.json); config/blobs/repo-bundle entries return with their features.
- TUI (`internal/tui`): Jobs and Backups screens, `exocomp tui`.
- `exocomp secrets add|list` seals values from stdin; playbook runs feed every
  sealed plaintext to the redactor.
- Deferred to M2, tracked: amendment-2 git-leg chunking and repo-size guard;
  S3 credentials as sealed rows (amendment 1); systemd-creds TPM sealing;
  age-plugin-yubikey recipients.

## 2026-08-21 — M1 security and DR corrections

An adversarial review of the M1 code found claims the implementation did not
keep. Corrections, code first:

- The recovery card told the operator to restore "using the read-only storage
  credential from this card". The card carries no such credential: per-target
  read-only keys are [amendment 1](design/amendments.md), scheduled for M2. The
  card now says the operator supplies the uri and its credential.
  [disaster-recovery.md](operations/disaster-recovery.md) still describes the
  amendment-1 end state, which is design, not current behaviour.
- `exocomp doctor --deep` does not exist. The card says `exocomp doctor`; the
  operations pages still name the designed `--deep` form.
- `install.sh --restore` without an exocomp binary printed "would restore" and
  exited 0 — a recovery entrypoint reporting success having done nothing. It now
  runs the card's chain in POSIX shell (age, openssl, restic, coreutils),
  verifies the restored files against MANIFEST.json, and fails non-zero at every
  step. `packaging/restore_test.go` drives it against a real backup.
  keyring.age is read from a local target root; an s3 or git target needs the
  operator to fetch keyring.age and pass `--keyring`.
- `backup canary` is gone; `secrets add` supersedes it (it was listed above as
  deferred to M2).
  age-plugin-yubikey recipients; `backup canary` removal in favor of `secrets add`.

## 2026-08-21 — M1 gate corrections

Adversarial review of the M1 gate found curated pages describing a not-yet-built
executor and CLI surface as though they already shipped (code wins, per
[AGENTS.md](../AGENTS.md)):

- **Executor substitution.** `requirements.md`, `overview.md` (the diagram node and
  a new explicit carve-out, below), `delivery.md`, and both affected
  `sbom/bill-of-materials.md` rows named `ansible-runner` with per-task JSON
  events as the run engine. M1's actual executor
  (`internal/executor/ansible.go`) is a direct `ansible-playbook` subprocess with
  stdout scanned line by line into events and exit-code-only correctness;
  `ansible-runner` is unused in M1 and re-evaluated for M2.
- **`operations/disaster-recovery.md` legs and failure-mode tables.** The git
  leg's 90 MB chunking and 4 GB size guard (amendment 2) are M2, not shipped —
  `internal/backup/git.go` pushes the archive whole with no size guard. Every
  command in the failure-mode table now matches the shipped CLI: `exocomp
  restore` takes `--from`/`--dest`/`--identity` and restores latest only (no
  `--snapshot` selector, no in-place restore); `exocomp recovery-kit` only
  re-renders stored kit structure and cannot mint or revoke a recovery identity;
  `exocompd serve` itself is not implemented in M1, so startup integrity
  gating, per-target alerting, the weekly recipient nag, and the bootstrap
  wizard are all M2.
- **`architecture/overview.md`'s "one queue, no side doors" invariant.**
  `exocomp backup run` and `exocomp restore` are direct CLI invocations in M1,
  outside the queue — there is no scheduler yet to enqueue them against, and
  the queue's guardrails do not apply to them. The page now states this
  explicitly as an M1 carve-out rather than leaving the exception silent; they
  become queued jobs once the M2 scheduler exists.
- **`api/` lands in M2, not M1.** `AGENTS.md`'s layout table and
  `internal/api/doc.go` both said the OpenAPI spec and typed client land in M1;
  neither exists yet. Corrected to M2.

## 2026-08-21 — operations pages aligned with the shipped surface

`doctor --deep`, `jobs list`, and the card-carried storage credential were still
described as current in three spots the M1 corrections missed; each now names the
shipped command or marks the capability M2.

## 2026-08-21 — MinIO assessment added

[sbom/minio-assessment.md](sbom/minio-assessment.md): AGPL-as-test-fixture imposes no
obligation; the real finding is CE's archival (2026-04). Fixture swap to SeaweedFS
backlogged for M2b/M3; BOM's operator-store row no longer suggests MinIO for new
deployments.

## 2026-08-21 — persona review synthesized

[design/user-feedback.md](design/user-feedback.md): four adversarial persona reviews
(veteran homelabber, SRE-at-home, time-poor generalist, security engineer). Five
convergent findings folded into M3 scope; ranked v1.1 backlog established;
[ADR 0007](adr/0007-audit-chain-deferral.md) records the previously silent audit-chain
deferral.
