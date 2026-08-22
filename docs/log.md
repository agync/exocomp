# Knowledge base evolution log

Structural history of this bundle: pages added, deprecated, moved, merged, or split, and
corrections of fact. **This is not a software changelog** — releases, features, and bug
fixes belong to the project's release notes, not here.

Newest first.

## 2026-08-22 — M3c plan policy: one operator page added

[operations/plan-policy.md](operations/plan-policy.md) is new: the CEL gate over a
workspace's `tofu show -json` plan. It documents the `plan` variable and the plan-show
keys a policy reaches (`resource_changes[]`, `resource_drift[]`, `.change.actions[]`),
compile-at-add refusal, the two actions (`deny` sealing a plan unconsumable so its apply
refuses, `require_approval` routing the apply through the existing gate), the operator
transcripts for each, and the boundaries: the verdict record carries only policy name and
action, evaluation re-describes the sealed plan offline from the plugin cache with no
backend or bearer, a workspace with no policies pays nothing, an unexaminable plan seals
fail-closed denied, and a raw plan-file apply is not gated. Linked from the operations
index between reacting-to-alerts and sources.

## 2026-08-22 — M3a reactive: two pages added, overview's carve-out closed

[architecture/reactive.md](architecture/reactive.md) is new: the webhook ingress and its
derived per-source path token, the one normalized event shape and why its dedup key
excludes the status, the sandboxed CEL environment and its time and cost budgets, the
guardrails in ledger order with the reason strings each writes, the approval token's
derivation and its mapping onto the six points of the approval surface contract, and
drift-as-an-event with the event table as its own checkpoint.

[operations/reactive.md](operations/reactive.md) is new: wiring Alertmanager,
healthchecks, and generic JSON senders; the sealed-secret names the ntfy transport reads
and why a bare public topic is refused rather than warned about; `rule`, `silence`,
`approval`, and `event` usage; and the `tofu-plan` schedule kind. Linked from
`architecture/index.md`, `operations/index.md`, and the top-level `index.md`.

[architecture/overview.md](architecture/overview.md) gained the paragraph closing the
other half of the one-queue carve-out: guardrails and approvals are enforced at enqueue,
and `awaiting_approval` is a state no claim statement admits rather than a UI convention.
## 2026-08-22 — M3a security folds: keyring generations, manifest tags, kit CLI

The five convergent findings of [design/user-feedback.md](design/user-feedback.md) that
touch keys and backups are implemented; the pages describing them are corrected here.

**The keyring generation lives beside the keyring, not inside it.** The design called for
a versioned keyring file. age rejects any framing added before or after an armored file —
verified against the `age` CLI both ways — and the printed card's
`age -d -i recovery.key keyring.age` is a CI-executed build artifact, so the counter rides
in `keyring.version` next to `keyring.age`: in the data dir, in the backup staging tree,
and at every target root. A directory with no such file is generation 1, which is every
data dir written before this.

[operations/disaster-recovery.md](operations/disaster-recovery.md) gains the two files the
staging tree grew (`keyring.version`, `MANIFEST.mac`), what a restore does with them —
authenticity before integrity, tampering reported apart from corruption, an untagged
snapshot restored with a loud warning — and a "Deriving without the daemon" section for
`exocomp kit derive`, `kit verify` and `identity migrate`.

**The card takes the design handoff's section layout**, numbered 1-6 with the
hand-written storage-credential box, the kit version line and the page digest. The
handoff's section 7 (dead-man ping) is *not* rendered: nothing configures a dead-man ping
yet, and a card section describing an unbuilt feature is the failure mode the M1 gate
corrections were about. It lands with the reactive layer. The derivation block renders
from the salt and purpose table in code, as the design-handoff entry below records, and
every command renders as `exocomp` ([ADR 0002](adr/0002-project-name-exocomp.md)).

[architecture/execution.md](architecture/execution.md)'s OpenTofu section states that a
saved plan is encrypted at rest to the keyring's recipients and decrypted into the run's
private directory for an apply or an approval diff. Plan artifacts are not part of the
backup staging tree, so the restore drill is unchanged.

The repository root gains a [DCO](../DCO); [AGENTS.md](../AGENTS.md) records that
contributing certifies it and that no sign-off trailer is used, invariant 9 admitting
none.

## 2026-08-22 — M2d reconciliation: serve, execution, and BOM corrected against code

[operations/serve.md](operations/serve.md): the daemon-aware CLI list gained `backup
run` and `playbook vendor`; the single-instance probe is now documented as shared and
non-blocking, with the acquire-side retry that keeps a probe from costing a starting
daemon the directory; startup's stale-lock warning and a "Known limits" section
(`SQLITE_BUSY` on the WAL pragma, the executor's ~2s pipe-drain grace) are new; a
"Schedules" section documents `--cron` validation and skip-on-reject reload behaviour.

[architecture/execution.md](architecture/execution.md): a "Queue semantics" section
states the one-shot-drain/no-daemon-rows rule and that `store.ClaimJob` enforces
concurrency keys in one SQL statement, not an in-process map. The executor tier table
now says what `local` actually ships (no per-job tmpfs, no separate executor UID) and
marks `local-sandboxed`/`container` designed for M3, with the trust-equivalence
consequence stated plainly. The stale-tflock paragraph flipped from aspiration to
implemented (`workspace unlock`, audited system event).

[adr/0004](adr/0004-opentofu-http-state-backend.md) gained a status note dating the
stale-lock recovery landing. [operations/disaster-recovery.md](operations/disaster-recovery.md)
re-dated its two lingering "M2" failure-mode rows to M3, neither matching a v1.1
backlog item. [sbom/bill-of-materials.md](sbom/bill-of-materials.md) refreshed
drifted Go module rows against `go.mod` and closed the ansible-runner
re-evaluation: staying with the direct driver, JSON callback plugin backlogged v1.1.

[design/delivery.md](design/delivery.md) marks v0.5/M2 landed 2026-08-22
(`test/e2e/m2-gate.sh`), splitting the full 8-screen TUI and wizard out to v1.0/M3
now that both are designed.

## 2026-08-21 — M2c: serve page added, backup run states corrected

[operations/serve.md](operations/serve.md) is new: `exocomp serve`'s flags
(`--port`, `--drain-grace`, `--reload-interval`), the signal contract for `SIGHUP` and
`SIGTERM`/`SIGINT`, the single-instance lock on `<data dir>/serve.lock`, and what
`workspace plan|apply` and `playbook run` do while a daemon is up — they enqueue and
follow the job instead of draining a queue of their own. Linked from
`operations/index.md` and the top-level `index.md`.

[operations/disaster-recovery.md](operations/disaster-recovery.md) gained the terminal
states a `backup_run` row can hold. `cancelled` is new — a leg stopped by a shutdown was
previously indistinguishable from one the target failed, and a row left `running` now
means only that the writing process was killed outright. Its failure-mode table said
twice that "`exocomp serve` currently serves the OpenTofu state backend and nothing
else"; that stopped being true when serve became the daemon, and both parentheticals are
gone.

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

## 2026-08-21 — daemon redaction resolves per job

The m2 gate caught the daemon snapshotting sealed secrets at startup: a secret
sealed after `exocomp serve` started was not redacted until restart. The queue
now takes a redactor provider resolved at job claim time
([operations/serve.md](operations/serve.md), "Redaction freshness").

## 2026-08-22 — approval surface contract adopted

The design exploration settled the phone approval flow's semantics; the seven-point
contract is recorded in [design/ui-brief.md](design/ui-brief.md) and binds the M3
web/approvals implementation.

## 2026-08-22 — ADR 0008: step ledger over DBOS

Durable-execution assessment recorded: DBOS Go+SQLite is the one invariant-
compatible engine but is deferred (young SQLite path, single-maintainer, second
runtime); M3 builds rotation/upgrade on a step-ledger extension of the job store.

## 2026-08-22 — ADR 0008 fact corrected

SQLite is DBOS's default system database, not merely optional (verified against
their docs); their Postgres-for-production advice is about multi-server
distribution, which this project's invariants exclude. The deferral rationale
(single-maintainer, second execution runtime) is unchanged.

## 2026-08-22 — ADR 0009: LAN access tokens

Operator decision: LAN exposure is secured by bearer tokens minted and managed
via local auth only, with default self-signed TLS and fingerprint pinning
([ADR 0009](adr/0009-lan-access-tokens.md)). Binds the M3 web lane and adds four
v1.0 gate assertions.

## 2026-08-22 — design handoff v0.1 imported

Tokens landed verbatim at `internal/web/tokens.css` (annotated contrast, split
accent, signal system, type ramp). Artboard captures + the canvas PDF live under
[design/reference](design/reference/). Two reconciliations recorded for M3:

- **The printed card's derivation block must render from code, not the mock.**
  The design shows a per-install random salt (section 3) and `info =
  "exocomp/v1/" + purpose`; the shipped envelope uses the constant salt
  `exocomp/v1` with `info = purpose:id` (ADR 0003, CI-executed card test). Code
  wins; the card layout is adopted, its derivation text regenerates from the
  implementation.
- **The mock's `exo` command shorthand does not exist**: ADR 0002 forbids an
  `exo` alias (exo-lang collision). The kit/wizard commands render as `exocomp`;
  the new surfaces the design assumes — `kit derive [--list]`, `kit verify`,
  `identity migrate` — are adopted into the M3 CLI scope.

## 2026-08-22 — TUI design settled

The TUI canvas answered its brief in full; adopted decisions and the one new
engine requirement (notification-mute) recorded in
[design/tui-brief.md](design/tui-brief.md).

## 2026-08-22 — design handoff v0.2 (TUI) imported

TUI round as text under [design/reference/tui](design/reference/tui/): frames,
keymap, colors, copy, states — the authoritative artifacts (PNGs for looking
only). The twelve frames are copied into `internal/tui/testdata/frames/` as
golden fixtures the M3 TUI View() tests assert against; all pass their declared
width invariant (80/120/60). Canvas PDF v0.2 committed. Both standing
corrections (`exocomp` not `exo`, derivation-from-code) honored by the handoff.
Tokens refreshed from source.
## 2026-08-22 — `local-sandboxed` shipped, and one ADR 0005 claim corrected

The nono tier is the default on Linux: per-kind profiles, a startup probe that
asserts rather than assumes, and a loud degradation to plain `local` on any host
that cannot enforce it. Three corrections the executed probe forced, code
winning over the design as usual:

- **A pathname `AF_UNIX` connect is not mediated by any Landlock ABI.** Landlock
  hooks path opening; `connect(2)` on a unix socket opens nothing. Abstract
  sockets are scoped and refused; pathname ones are not. ADR 0005's socket
  denial now holds by placement — no exocomp unix socket exists for a job to
  name, the daemon's own surface is loopback TCP, no podman socket is mounted —
  and `doctor` prints the gap instead of implying a denial. The ADR's insistence
  on asserting is what found this.
- **Egress is filtered by port, not by destination.** Landlock's network rules
  match the TCP port alone, and nono's per-domain filtering routes through an
  HTTPS-terminating proxy — unusable for SSH, wrong for provider signature
  verification. Ansible gets TCP 22, tofu gets 443 plus the loopback backend
  port. Which *hosts* a job reaches stays a network-layer question.
- **`<data dir>/run` must stay under 57 bytes.** The tier redirects `TMPDIR`
  beneath it and the tools there coordinate over unix sockets placed inside it;
  `AF_UNIX` addresses cap at 108. The probe measures it and degrades loudly
  rather than letting each job discover it as an unexplained timeout.

## 2026-08-22 — image toolchain baked in (ADR 0006)

`packaging/Containerfile` builds the real Python layer: uv-managed CPython 3.14.0
plus `ansible-core==2.21.3` with `ansible-runner==2.4.3`, resolved through the
committed `toolchain-constraints.txt`. tofu, restic, age, sops, and nono are
fetched by sha256 from `tool-digests.txt` for both architectures; git alone comes
from Fedora's signed repository, because upstream publishes no binary release to
pin, and its resolved version is recorded in the image's `MANIFEST.json`.
`make budgets` measures the two numbers the bill of materials commits to and
exits non-zero above either ceiling: 592 MB against 800, 17 MB idle RSS against
80. The uv escape hatch stays opt-in — the image's `UV_TOOL_DIR` is baked, not
the volume, because a fresh restore has the image before it has the volume.

## 2026-08-22 — Ansible & OpenTofu expert reviews

Two practitioner reviews ([design/expert-reviews.md](design/expert-reviews.md))
converge: the execution cores are correct but the credential plane binding sealed
secrets to run env (provider env / plugin env / TF_VAR_) is missing, and the
sandbox allowlist starves dynamic inventory and providers. v1 must-fixes fold into
the M3b source-model scope; both lists recorded.

## 2026-08-22 — M3b glue: run-path credentials and v1 surfaces

The credential-plane binding is wired end to end: a run carries its `run_secret`
binding and inventory names on the job row and resolves them to named env at
execution — sealed values decrypted through the envelope for that run alone,
absent from the row and scrubbed from events. This is what lets a dynamic
inventory or a provider authenticate under the sandbox (stories 1/3/4/5).
[operations/sources.md](operations/sources.md).

Surfaces and deferred must-fixes landed with it: `workspace add --state-only`
emits the mode-A backend block + bearer; `workspace prune` applies a keep-last-N
+ last-M-days state policy; the web dashboard shows MUTED (jobs still run)
distinct from PAUSED; `serve --web-addr` runs the token-gated TLS LAN surface
beside the loopback state backend ([ADR 0009](adr/0009-lan-access-tokens.md));
the offline tofu path regenerates the lock from the fs-mirror with `h1:` hashes;
`host onboard` records `known_hosts` into the backed-up store, ensures the fleet
key, and seeds the onboarding runbook; and setup step 4 persists the operator
name and ntfy topic and sends (or clearly declines) the test push. Operator
pages: [sources](operations/sources.md), [LAN access](operations/lan-access.md),
[setup and notifications](operations/notifications.md).
