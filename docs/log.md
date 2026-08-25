# Knowledge base evolution log

Structural history of this bundle: pages added, deprecated, moved, merged, or split, and
corrections of fact. **This is not a software changelog** — releases, features, and bug
fixes belong to the project's release notes, not here.

Newest first.

## 2026-08-25 — phone-companion API round 2

- **`GET /whoami` now carries `server: {version, hostname}`.** The daemon's build version
  (`internal/version`) and its host ride the identity reflection, so the Connect screen can
  render `exocompd <version> · <hostname>` without fishing the host out of the URL. Both
  fields are omitempty; a missing hostname degrades the line, never the request.

## 2026-08-25 — QR pairing, schedule detail, sidebar logo

- **QR device pairing.** The token-mint result (web) and `exocomp token new` (CLI) render a
  pairing QR carrying `{v, url, token, fingerprint, scope, name}` — a phone scans it to
  connect and TOFU-pin the certificate. A shared `internal/pairing` package renders an SVG
  for the web and a half-block terminal QR for the CLI; the base URL resolves from
  `daemon.public_url` (set by `serve --public-url` or the System page) else the request
  host. New [operations/pairing.md](operations/pairing.md) documents the payload contract.
- **Schedule detail.** A schedule row opens a spec modal — kind, cron, command, target,
  concurrency key, jitter, enabled, next/last fire, and last outcome.
- **Sidebar brand mark** is rendered once, in a fixed column centered on the nav-glyph
  column, so it no longer shifts or resizes between the expanded sidebar and the rail.

## 2026-08-25 — phone-companion API surface

- **Four new `/api/v1` endpoints** land the mobile handoff: `GET /whoami` reflects the
  caller's own token; `GET /approvals/{id}/plan` opens the sealed plan and returns it as a
  structured, redacted diff (`destroys`/`changes`/`creates`) through the same
  `PlanOpener.OpenSealedFile` + `classifyPlan` core the web approval surface renders, so a
  secret-backed value the web redacts is never carried as plaintext, gated by the
  plan-view floor; `POST /jobs/{id}/cancel` moves a non-terminal job to `cancelled` (409
  terminal, 404 unknown); `POST`/`DELETE /silences` mute/unmute through the store. All four
  are operator, bearer-only.
- **Approvals SSE.** `GET /api/v1/approvals/stream` mirrors the job stream's
  registration/gating and per-poll revocation check, but has no terminal state — it sends
  one snapshot on connect and another only when the pending set changes, replacing a
  phone's poll of `GET /approvals`. SSE `ts` now formats with the same RFC3339 the REST
  views use, so one parser covers both. [operations/api.md](operations/api.md) updated.
- **ntfy privacy toggle.** A `notify.ntfy_detail` setting switches the ntfy transport
  between `descriptive` (today's title/body/deep-link) and `terse` (a fixed generic
  phrase) for operators who don't trust their relay.

## 2026-08-25 — job detail live-update fix

- **SSE backfill.** The job-detail stream started its cursor at the latest event on
  connect, skipping everything written between page render and connect — a fast job stuck
  on QUEUED with no lines until reload. The client now passes `?since=<last-rendered-seq>`
  to backfill the gap, and reloads on `done` to render the true terminal status, full log,
  and actions.
- **Schedule button** moves from an always-shown page action to a small icon button in the
  header, enabled only after a successful run.

## 2026-08-25 — TUI palette matches design

The TUI color table realigns to the design handoff: magenta accent for keys/links/the
active tab (was terminal blue), green for both succeeded and running, a new yellow for
`awaiting_approval`, and the design's fail/heading/dim hexes. Truecolor values with
lipgloss's ANSI fallback; color stays a hint — every state still reads as its word.

## 2026-08-25 — web UI: themed select, dialog modal, System page

- **Themed `<select>`.** Every `.writeform` select gets an accessible themed listbox
  overlay (native select kept as the real form field and the JS-off fallback), so option
  lists follow the app theme cross-browser instead of the OS default.
- **Dialog modal rebuild.** The modal moves onto the native `<dialog>` element (top
  layer), fixing an overlay pointer-events bug that blocked opening a select, focusing
  inputs, and backdrop/close-button dismissal.
- **System page.** A new `/system` page holds the control-plane health card, backups, and
  the server version — moved off the dashboard and the old footer. A fixed-bottom
  powerline replaces the end-of-content footer, carrying the liveness signals and the
  signed-in operator. Nav reorders Dashboard-first then alphabetical, with a divider
  before System.
- **Sidebar collapse toggle** moves to the top of the sidebar (Gemini-style); expanded, it
  sits at the right edge, and the collapsed rail rests on the exocomp brand mark —
  hovering or focusing swaps the mark for the open-chevron affordance, with the mark
  living inside the toggle button so the control stays keyboard-reachable.
- **Workspaces Configure modal.** The page-level how-to card and inline "add state" form
  are dropped; a Configure modal presents OpenTofu/Terraform tabs with a single generic
  intro (stable modal size across tabs) instead of per-tab prose, and each backend-config
  code block scrolls inside its own box (a long address no longer widens the modal) with a
  copy-to-clipboard button. The list section renames to "States"; registering a
  state-only workspace from the web is retired — `workspace add --state-only` is CLI-only
  now.

## 2026-08-25 — dependency resolver sandbox grants uv config

`uv`'s config discovery is mandatory and treats an unreadable config as fatal; the
default-deny resolver sandbox profile hid the host's `/etc/uv/uv.toml`, so a job's
`uv venv` died with EACCES. The sandbox now grants the existing system/user `uv.toml`
read-only (host root stays out of scope), so the resolver reads the config the operator
already trusts instead of failing closed on it.

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

- **Confused-deputy guard.** `resolveRunSecrets` refuses to inject a sealed value shaped
  like an exocomp access token (`exo_<id>_<secret>`) into a job's env, closing the path
  where an operator binds the daemon's own credential as a run-secret and turns a job into
  a deputy for the API. One choke point covers every job driver.
  [security/threat-model.md](security/threat-model.md) D2 updated.
- **Threat model renumbered.** The internal E/S/R/D/B/U/AI finding-id families (gaps,
  out-of-order entries, orphan residual refs) are replaced by a single sequential T1-T29
  catalog in document reading order; residual bullets lead with a short descriptive phrase
  instead of a dangling id.
- **Empty `--inventory` defers to ansible.** `AnsibleArgv`/`AnsibleAdhocArgv` no longer add
  `--inventory` when none is given, so ansible discovers its own (the checkout's
  `ansible.cfg`, then its default hosts file) instead of being pinned to `localhost,`. The
  venv-interpreter force now applies only to an explicit `localhost,` inline inventory.
  [operations/adhoc-and-partial.md](operations/adhoc-and-partial.md) and
  [operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md) updated.
- **Token rotate refuses self.** Mirrors the existing revoke self-guard: rotating the token
  that authenticates the current session is rejected with 409 before reauth, since rotation
  would invalidate the secret making the request.
- **Cache-busted assets.** `/assets/*` links carry a content-hash version query
  (`app.css?v=<hash>`), so a browser holding a prior build's CSS under `max-age` no longer
  renders fresh markup against stale styles.

## 2026-08-25 — sidebar nav, dashboard, and shared modal

- **Sidebar nav.** The horizontal title-bar nav (trailing items scrolled off behind a fade)
  is replaced by a left sidebar with a logo mark, a glyph per section, a desktop icon-rail
  collapse (persisted), and an off-canvas drawer under 820px. Marketing tagline kickers
  ("Needs you", "Everything is a job", etc.) are dropped in favor of the section's own
  heading; functional kickers stay. Nav and brand links no longer take the visited-link
  purple or a hover underline — ink stays the resting/visited color, accent marks only the
  current or hovered item.
- **Dashboard.** Cards pack into an auto-fill responsive grid instead of a fixed 2-column
  layout that left sparse cards as half-empty rows, and cards in a row stretch to equal
  height. Card order now leads with infra actions (approvals, queue, workspaces, workflows,
  sources, schedules); backups and the control-plane health card move to the bottom, and the
  OpenTofu-drift card is renamed Workspaces so ansible and tofu read as equal citizens. Rows
  in the Queue/Workflows/Jobs/Events lists carry `data-href`, so a click anywhere in the row
  navigates, without disturbing the existing id link or keyboard access.
- **Chrome fixes.** The theme ground (`html{background}`) now paints from an inline
  pre-CSS `<style>` so the browser's white default never flashes during the load gap; `main`
  fills the content column at full width instead of capping at 1100px and leaving side
  gutters; the job-detail live tail renders one event per line with a dimmed timestamp
  instead of walling repeated stamps; the theme toggle becomes a compact icon button
  matching the sidebar collapse control.
- **Shared modal.** A `<dialog>`-style modal component (focus trap, save/restore, a `+`
  button beside a list page's `<h1>`) replaces the always-visible inline add-form card on
  secrets, tokens, runners, policies, and schedules. Source add collapses to one
  auto-detecting field (`source.LooksLikeGitURL` routes to the path or git body); a job's
  detail page gains Schedule (prefilled from the job's own runbook/kind/argv) and Run-again
  actions.
- **Workspaces reframed.** The Workspaces page becomes a managed-OpenTofu-state view: an
  explainer, a per-row Backend config modal with a paste-ready backend `http` block, and a
  state-only add form (`POST /workspaces`, matching `workspace add --state-only`).
  [operations/sources.md](operations/sources.md) notes the web UI names this "Workspaces"
  while the CLI verb and store/API keep their existing shape.
- **CLI.** `--help` groups the ~48 top-level verbs into seven titled sections (run, sources,
  secrets, automation, hosts, backup, service) — presentation only, the `commands` machine
  tree is unchanged. `token rotate <name>` reseals a fresh secret onto the existing row
  (keeping id/name/scope for audit continuity) and drops sessions from the old secret; the
  `token new` name-collision error now names the revoked holder and points at `rotate` and
  `token list --all`. A new `RunAdhoc` service verb assembles the ansible ad-hoc queue spec
  and gates it through approval like `ApplyWorkspace`.

## 2026-08-25 — source kinds collapsed; runs move to the source page

- **Source kinds** are now `git` and a single `path` (an absolute path, or one relative to the
  data volume), with a `writable` flag (default read-only, now enforced in the sandbox). The
  `hostpath`/`volpath` split is gone. [operations/sources.md](operations/sources.md) and the
  [ADR 0010](adr/0010-source-and-credential-model.md) addendum carry the model.
- **Runs are launched from a source's page.** Each source has a playbook/command launcher; the
  standalone run surface is retired. Reflected in [operations/serve.md](operations/serve.md).

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
