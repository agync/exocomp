# Knowledge base evolution log

Structural history of this bundle: pages added, deprecated, moved, merged, or split, and
corrections of fact. **This is not a software changelog** — releases, features, and bug
fixes belong to the project's release notes, not here.

Newest first.

## 2026-09-01 — memfd run-secret staging rejected, recorded

- **A `memfd_create(2)` staging experiment was revisited and rejected.** A
  long-dormant branch staged each run-secret into an anonymous memfd (no path in
  any mount, `F_ADD_SEALS`, mlock) instead of the shipped `/dev/shm` file
  staging (`--secret-staging {auto,tmpfs,disk}`). The branch was 265 commits
  behind and never merged; the shipped design already absorbed its rationale and
  then superseded it.
- **Why rejected (code wins).** A memfd's fd does not survive a sandboxed run:
  real `nono` closes every inherited descriptor but stdio before exec, and a
  memfd has no path a `--read-file` grant can name. A sandboxed tofu child can
  read a staged file under `/dev/shm` through the granted path — the exact case
  the memfd design could not support (`internal/executor/sandbox_e2e_test.go`).
- The branch is deleted; the `/dev/shm` staging with its loud disk fallback
  stands.


## 2026-09-01 — list-surface refinements

- **Docs' UI-stack references corrected.** The vendored htmx include is gone; the docs now name the vanilla JS the app actually uses.

## 2026-08-31 — approval surface refinements

- **Log self-correction.** Three back-dated entries (list-surface refinements, write-form errors, check-event traceability) now carry their true 09-01 dates; newest-first order restored.

## 2026-08-31 — sweep error policy recorded

- **[architecture/reactive.md](architecture/reactive.md) states the shared
  sweep error policy** — the checks, drift, and job-failures sweeps now say
  in-place that a per-item failure is logged and the sweep continues (a failed
  check is still marked due and keeps its interval cadence; a broken check
  target is skipped so its siblings still probe), and only a structural failure
  (the sweep's list query) or shutdown stops it.


## 2026-08-31 — docs-review pass on checks, rules, and the web

- **Corrected a false claim about `doctor`.** [security/threat-model.md](security/threat-model.md)
  T33 and [design/check-triggered-runs.md](design/check-triggered-runs.md)
  §3.2.1 said `doctor` asserts the LAN-only-routing precondition; it has no
  such probe. Both now record the precondition as documentary, not enforced.
- **[operations/reactive.md](operations/reactive.md) "Checks as an event"
  states the consequences the feature ships with:** a check's interval is
  floored at the `--sweep-interval` tick (30s default); a host home at daemon
  start is missed once, fail-safe; hysteresis is fixed at N=3 and a departure
  withdraws a pending approval only after 3 consecutive down observations
  (~N·interval lag on the resolve path); the persisted `check_state` row
  (status/consecutive/emitted/last_change) survives a restart and backs the
  /checks "changed N ago" line; the two §3.2.1 preconditions (static lease,
  no overlay route) are stated where an operator sets up; and the http
  dial-time guard plus the no-netgroup-on-url caveat are on the page.
- **The web paragraph and [operations/serve.md](operations/serve.md) browser
  list gain the shipped surfaces** — /checks netgroup chip + recency, the
  dashboard Checks card, job detail "fired by rule" → /rules + the triggering
  event, the events `?source=` filter, and the read-only /rules page.
  [operations/api.md](operations/api.md) adds `GET /api/v1/netgroups` and
  `GET /api/v1/checks` to the read endpoints.
- **[design/check-triggered-runs.md](design/check-triggered-runs.md) §4.2
  check_state DDL reconciled to migration 0030** (up/down, consecutive,
  emitted; no `unknown` — a missing row is the unobserved state), §5.1 http
  is 2xx/3xx, §9/Status note rules read-only on the web, and the Open
  questions close: hysteresis N fixed at 3, retention shipped last-status-only,
  the http-tailnet caveat, and cooldown keying via `0031_rule_fire_dedup.sql`;
  set-event / `--limit-hosts` stays open.


## 2026-08-31 — CLI/web check, rule, netgroup surfaces consolidated

- **Correction of earlier framing.** The "store remains the shared validator" claim is corrected: the CLI and web parsers now unify in store.Parse* / internal/reactive.

## 2026-08-31 — threat model: checks and peer stamping rows

- [security/threat-model.md](security/threat-model.md) gains a **Checks, netgroups, and peer
  stamping** subsection under the reactive section — T30–T35 record the shipped mitigations
  (peer-stamped presence, transition-only + hysteresis + per-target cooldown, the static-lease
  and LAN-only-routing preconditions, cold-start seeding, http status-only) that the code
  already carried, closing the invariant-8 gap the feature's design §10 owed. New rows are
  numbered T30+ to avoid renumbering the existing T1–T29 catalog.
- [index.md](index.md) summary rows for the Reactive layer and Reacting to alerts now mention
  check probes, netgroups, and peer stamping.


## 2026-08-31 — contribution guide and specs store

- **New page** [contribution/index.md](contribution/index.md): the working
  procedure behind [AGENTS.md](../AGENTS.md) — the DCO, commit conventions,
  worktree workflow, verification, and design/review. Linked from the bundle
  index and from AGENTS.md's "Working here".
- **`brain/` lands in `.agents/`** — `.agents/brain/specs/` holds specs for
  in-flight work (the UI spec for check-triggered runs is the first). It is
  tracked, like the rest of `.agents/` content (skills, role cards,
  workflows); the git-excluded half of `.agents/` stays the generated workflow
  scripts. AGENTS.md's layout block names the new directory.


## 2026-08-31 — web surface for netgroups and checks

- **Design reconciled.** check-triggered-runs.md §9 drops the in-browser rule-builder promise and records the shipped-subset Status; architecture/reactive.md and operations/reactive.md gained checks/netgroup sections.

## 2026-08-30 — check-triggered runs design

- **New page** [design/check-triggered-runs.md](design/check-triggered-runs.md):
  proposal (no code) for running governed jobs on a reachability or health
  transition instead of only a wall clock. Three primitives: **checks** (named
  probe-sets — `tcp`/`ping`/`http`, transition-only events, per-target dedup),
  **network groups** (named CIDR sets giving the operator's own vocabulary and
  classifying a probe-target or webhook-peer address), and a governed
  **`playbook` rule action** (source checkout, inventory, run-secrets, sandbox)
  replacing the shell-argv escape hatch. Checks enter the existing reactive
  pipeline the way drift does (`SweepDrift` precedent), so cron stays on the
  schedule row and a check is a rule listening on `--source check`; both
  converge on the one queue.
- **Revised after review.** Address = location now states its two preconditions
  (static lease per target; LAN-only routing, never an overlay subnet route) and
  both get a threat-model row. Transition state lives in a `check_state` table
  with cold-start seeding that never emits on first observation, so a restart
  does not fire every presence rule. Phase 2 ships per-target events only; the
  set-event / `--limit-hosts` expansion is deferred. Peer stamping is framed as
  a network-path signal (LAN vs overlay), not a location verdict. Check
  transitions require N-consecutive-same-result hysteresis. Cooldown stays
  per-rule for webhooks (uniform per-dedup-key would let a body-varying webhook
  bypass the throttle) and is per-target for sweep sources.


## 2026-08-25 — QR pairing, schedule detail, sidebar logo

- **New page** [operations/pairing.md](operations/pairing.md) documents the QR device-pairing payload contract.

## 2026-08-25 — threat model: local-exec CLI residual

[security/threat-model.md](security/threat-model.md) corrects the token-mint boundary: a
job can reach the `exocomp` CLI via an ansible `command`/`local_action` or a tofu
`local-exec`; under the sandboxed tier the data-dir/keys denial (T6) fails it closed, but
under `TierLocal` (explicit or auto-degraded) it can mint an operator token or read
`host.key` — documented as a trusted-playbook tier. Also notes the web `POST /tokens/new`
mint route exists, gated by cookie-session + same-origin + reauth; `/api/v1` and `/mcp`
expose no mint.


## 2026-08-25 — sandbox tiers renamed, secure-by-default

- **Tiers renamed and the default flipped.** The job-sandbox tiers are now `opportunistic` /
  `enforcing` / `disabled` (was `auto` / `local-sandboxed` / `local`), and the default — an
  empty `--sandbox`/`$EXOCOMP_SANDBOX` — is now `enforcing`: a host that cannot enforce the
  sandbox fails closed at `serve`/`doctor` startup rather than running jobs unsandboxed.
  `opportunistic` is the opt-in that degrades loudly to `disabled` instead, and `disabled` is
  the explicit unsandboxed tier. The three old names stay accepted as silent back-compat
  aliases. Terminology reconciled across [architecture/execution.md](architecture/execution.md),
  [operations/serve.md](operations/serve.md), [design/byo-runner.md](design/byo-runner.md),
  [security/threat-model.md](security/threat-model.md),
  [adr/0005-job-sandboxing-nono.md](adr/0005-job-sandboxing-nono.md), and the index, runners,
  cli-run, provision-converge, and SBOM pages.


## 2026-08-25 — access-token and inventory hardening

- **Threat model renumbered.** The E/S/R/D/B/U/AI finding-id families were replaced by a sequential T1–T29 catalog in reading order.

## 2026-08-25 — source kinds collapsed; runs move to the source page

- **ADR 0010 addendum** and operations/sources.md updated for the git/path (+writable) model; serve.md reflects the source-page launchers.

## 2026-08-25 — browser management surface documented

[operations/serve.md](operations/serve.md) gains a "The browser surface" section: the web UI
is no longer view-only — an operator runs playbooks/commands, plans/applies workspaces, and
manages sources, secrets, tokens, schedules, policies, and runners from the browser. Sensitive
writes re-confirm with the operator's access token; these session form posts stay distinct
from the bearer-only `/api/v1`.


## 2026-08-24 — design system page and top-level README

- **New page** [design/DESIGN.md](design/DESIGN.md): the web UI design system — color,
  typography, spacing, and component tokens — sourced from `internal/web`. The architecture
  technical-design that previously held this name moves to
  [history/design/DESIGN.md](history/design/DESIGN.md) as provenance.
- **New** repo-root `README.md`, current to shipped reality (OpenTofu, the single
  MCP-over-HTTP surface, remote CLI, sandbox tiers).
- **Voice.** Internal build codenames — milestone tags, finding and review ids, process
  jargon — are removed across the docs and ADRs; pages describe current behaviour and read
  harness-neutral.


## 2026-08-24 — Personas consolidated

The four target-user personas now live as one crisp definitions page,
[design/personas.md](design/personas.md). The internal persona-review, persona-scenario,
and practitioner-gap-review planning pages are removed; their surviving conclusions live in
[design/v1.1-roadmap.md](design/v1.1-roadmap.md) and
[design/v1-acceptance.md](design/v1-acceptance.md).


## 2026-08-24 — failover page and cross-host lease

- **New page** [operations/failover.md](operations/failover.md): the cold-standby
  procedure (restic restore, optional Litestream fast-forward, deliberate `serve`, re-drive
  of interrupted work) and the cross-host active lease that fences it above the host-local
  `serve.lock`.
- **Correction.** The lease fences only two daemons opening the *same* `exocomp.db` (a
  shared volume or network filesystem); it does not fence the Litestream-replica or
  restic-restore copies, each of which is a separate file. The runbook's restore path relies
  on the operator confirming the primary is stopped.
- [operations/serve.md](operations/serve.md) gained the cross-host half of "one daemon per
  data dir".


## 2026-08-24 — runner caps documented

[design/byo-runner.md](design/byo-runner.md) and
[operations/runners.md](operations/runners.md): per-job memory/CPU caps are enforced by
cgroup v2 direct writes (the container image ships no systemd, so `systemd-run` is not the
path), opt-in and fail-closed — the default rootless deployment refuses a capped job rather
than running it uncapped. A job selects a runner's caps with `--runner`. The
container-image tier stays deferred.


## 2026-08-24 — run-secret staging documented

[design/bootstrap-first.md](design/bootstrap-first.md) and the operations pages describe
run-secret file staging on `/dev/shm`: per-instance session directories, a
`--secret-staging {auto,tmpfs,disk}` backend with a loud disk fallback, best-effort mlock,
and unlink-on-exit. `exocomp doctor` reports the resolved backend and residual posture.


## 2026-08-24 — threat-model corrections

[security/threat-model.md](security/threat-model.md):

- DEK reseal (recipient revocation) is crash-safe — staged-then-promoted, with a leftover
  `keyring.age.next` reconciled on the next `Open`. The residual (a transit-sealed store
  cannot reconcile without the host key) is recorded open.
- The auth-timing decoy KDF is removed; the per-source/per-token fail counter remains the
  guard. The accepted residual timing gap is recorded, owned by `internal/web/auth.go`.


## 2026-08-24 — durable approval and scheduler corrections

- [operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md) and the
  bootstrap/handoff/provision pages: a composed-chain `--require-approval` step is held
  mid-run as a durable per-step approval, not a CLI confirmation.
- [operations/serve.md](operations/serve.md): schedule catch-up now dedups every fire on
  the occurrence's scheduled instant, and `last_run` advances only once a fire is enqueued,
  so a crash no longer double-fires and a failed enqueue is not silently dropped.
- [operations/reactive.md](operations/reactive.md): `approval link` needs its own
  `--public-url`/`EXOCOMP_PUBLIC_URL`; it cannot read the running daemon's value back.


## 2026-08-23 — agent-facing surface pages

- **New pages**: [operations/remote-access.md](operations/remote-access.md) (remote CLI,
  certificate pinning, `--remote`/`--token`), [operations/api.md](operations/api.md) (the
  `/api/v1` read and write surface, wire envelope, error-code table),
  [operations/machine-output.md](operations/machine-output.md) (`--output json`, the error
  taxonomy, and the honestly partial exit-code wiring), and
  [operations/runners.md](operations/runners.md) (the runner registry as definitions).
- **Corrections.** [architecture/overview.md](architecture/overview.md): `internal/service`
  owns the write verbs, not "all business logic" (reads stay store-direct); the loopback
  state backend and the operator `/api/v1` surface are separate rows.
- [operations/sources.md](operations/sources.md) documents `secrets rotate`/`rm`,
  `--generate`, `--expires`, and that expiry is a hygiene marker that never gates decryption
  or injection.


## 2026-08-23 — the single MCP-over-HTTP surface

The stdio MCP server documentation is removed. [operations/mcp.md](operations/mcp.md) now
describes one programmatic surface — MCP over HTTP on the `serve` listener, bearer-scoped
([ADR 0009](adr/0009-lan-access-tokens.md)) — with an off-the-shelf http-to-stdio bridge for
stdio-only clients and the agent-aware CLI as the local path.
[adr/0014-agent-facing-surface.md](adr/0014-agent-facing-surface.md) and
[design/agent-interface.md](design/agent-interface.md) are reconciled to this; several pages
(threat model, architecture overview, operations index) were caught up to the removal.


## 2026-08-23 — bootstrap and handoff pages

- **New page** [operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md):
  the `exocomp bootstrap run <playbook> <workspace>` ansible-first chain, with the
  `--capture` (env) and `--capture-file` (KUBECONFIG-shaped file) credential handoffs added
  in later passes.
- **New page** [design/bootstrap-first.md](design/bootstrap-first.md): the proposed
  ansible-first-then-tofu design for the Kubernetes persona, linked from the v1.1 roadmap.
- **New page** [operations/sealed-tofu-outputs.md](operations/sealed-tofu-outputs.md): the
  credential half of the Terraform→Ansible handoff (`tf-output/<workspace>/<output>` sealed
  and bound under `tf:<workspace>`).
- **New page** [operations/cli-run.md](operations/cli-run.md): the governed `cli` run kind —
  an allow-listed binary under the queue, sandbox, run-secret injection, and approval gate.
- **Corrections.** [operations/terraform-ansible-handoff.md](operations/terraform-ansible-handoff.md)
  and [operations/provision-converge.md](operations/provision-converge.md) updated as the
  daemon's apply began sealing sensitive outputs on every path; `bootstrap-first.md` Status
  updated as its phases shipped (code wins over the design page).


## 2026-08-22 — reactive, policy, and run pages

- **New pages**: [architecture/reactive.md](architecture/reactive.md) (webhook ingress,
  normalized event shape, sandboxed CEL, guardrails, the approval token),
  [operations/reactive.md](operations/reactive.md) (wiring senders, `rule`/`silence`/
  `approval`/`event`, the `tofu-plan` schedule), [operations/plan-policy.md](operations/plan-policy.md)
  (the CEL gate over `tofu show -json`), [operations/playbook-run.md](operations/playbook-run.md)
  (ansible passthrough flags and check-mode drift), and
  [operations/provision-converge.md](operations/provision-converge.md) (the
  provision→onboard→converge chain and the `tf:<workspace>` standing inventory).
- The operations index gained rows for the v1.0 pages that had shipped unlinked:
  [run-lifecycle.md](operations/run-lifecycle.md),
  [terraform-ansible-handoff.md](operations/terraform-ansible-handoff.md), and
  [upgrade.md](operations/upgrade.md).


## 2026-08-22 — design handoffs imported

- Web design handoff under [design/reference/](design/reference/) (canvas PDF v0.1, artboard
  captures); design tokens live at `internal/web/tokens.css`.
- TUI design handoff under [design/reference/tui/](design/reference/tui/) (text frames,
  keymap, colors, copy, states); the frames are golden fixtures under
  `internal/tui/testdata/frames/`. [design/tui-brief.md](design/tui-brief.md) and
  [design/ui-brief.md](design/ui-brief.md) record the settled decisions, including the phone
  approval-surface contract.
- **Correction.** The printed recovery card's derivation block renders from code, not from
  the mock: the shipped envelope uses the constant salt `exocomp/v1` with `info = purpose:id`
  ([ADR 0003](adr/0003-secrets-envelope.md)); the mock's `exo` command shorthand is not used
  ([ADR 0002](adr/0002-project-name-exocomp.md)).


## 2026-08-22 — ADRs 0008 and 0009 recorded

- [ADR 0008](adr/0008-step-ledger-over-dbos.md): durable execution is a step-ledger extension of the
  job store; DBOS is assessed and deferred (young SQLite path, single maintainer, second
  runtime).
- [ADR 0009](adr/0009-lan-access-tokens.md): LAN exposure is secured by local-minted bearer
  tokens with default self-signed TLS and fingerprint pinning.


## 2026-08-22 — keyring-generation and v0.5 reconciliation

- **Correction.** The keyring generation counter lives beside the keyring in
  `keyring.version`, not inside the armored `keyring.age` (age rejects added framing).
  [operations/disaster-recovery.md](operations/disaster-recovery.md) documents
  `keyring.version` and `MANIFEST.mac`, what a restore does with them, and
  `exocomp kit derive`/`kit verify`/`identity migrate`.
- v0.5 reconciliation against code across [operations/serve.md](operations/serve.md),
  [architecture/execution.md](architecture/execution.md), and
  [sbom/bill-of-materials.md](sbom/bill-of-materials.md): queue semantics, executor tiers as
  actually shipped, stale-lock recovery, and drifted module rows corrected.


## 2026-08-21 — Bundle created

The `docs/` bundle was created as an OKF v0.2 knowledge base, curated from the
pre-repository design archive. The curated pages summarize; the archive's source documents
were imported byte-identical under [history/](history/index.md) — thirteen files, checked
for absolute paths and home directories (none present).

- **Sections and pages**: `index.md`, this `log.md`, `adr/` (0001–0006, Accepted),
  `architecture/` (`overview.md`, `platform-contract.md`, `execution.md`), `design/`
  (`requirements.md`, `amendments.md`, `delivery.md`), `operations/` (`bootstrap.md`,
  `disaster-recovery.md`), `sbom/` (`bill-of-materials.md`), and `history/`.
- **Naming.** Most of the archive predates the rename from the working name "control" to
  **exocomp** ([ADR 0002](adr/0002-project-name-exocomp.md)); curated pages use the current
  name throughout, and imported history was deliberately not rewritten (see
  [history/index.md](history/index.md)).
- **Precedence.** Where a curated page and a history page disagree, the curated page wins;
  the twelve [amendments](design/amendments.md) override the accepted candidate design.


## 2026-08-21 — early corrections against code and source

- The design critique enumerates **30 gaps across sections A–H**, not 22; the wrong count
  was corrected on seven pages.
- `install.sh` gained the documented `--restore` and `--rollback --image-tag` forms,
  resolving contradictions with the disaster-recovery, bootstrap, and amendments pages in
  code's favor.
- v0.1 pages reconciled against the shipped code: the run engine is a direct
  `ansible-playbook` subprocess (not `ansible-runner`), the disaster-recovery staging tree
  and failure-mode tables match the shipped CLI, and `exocomp backup run`/`restore` are
  documented as direct invocations outside the queue until the scheduler exists.
- **New page** [sbom/minio-assessment.md](sbom/minio-assessment.md): the S3 test fixture
  moved to SeaweedFS; the operator-store row no longer suggests MinIO for new deployments.
