# Knowledge base evolution log

Structural history of this bundle: pages added, deprecated, moved, merged, or split, and
corrections of fact. **This is not a software changelog** — releases, features, and bug
fixes belong to the project's release notes, not here.

Newest first.

## 2026-08-31 — approval surface refinements

- **The approval phone now ticks and explains itself.** The countdown div
  carries the expiry as a machine deadline (`data-deadline`, unix millis) and a
  client clock re-renders "Xm Ys left" each second, flipping to "Expired — the
  window closed" at zero; the server stays authoritative on expiry and the
  JS-off initial text remains. The provenance sentence names the triggering
  alert and the rule it matched when the job's event row exists, and keeps the
  standing sentence when none does.
- **The approval surface fits the desk.** `.phone` widens to 720px at ≥821px
  while small screens keep the 390px column, and the mobile (≤560px) table-row
  button floor is now the hard 48px a11y target.

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

## 2026-08-31 — gating split: fast commit gate, full race gate

- **The per-commit gate is now fast.** `make check` runs `fmt-check vet lint
  tidy-check` plus `go test -short ./...`; the heavy race suite moved to a new
  `make check-race` target (`-race ./...`) that runs from the new pre-push hook
  and as the worktree-merge integration gate. Invariant 12 and the contribution
  guide describe the split; the heavy integration tests skip under
  `testing.Short()`.

## 2026-08-31 — CLI/web check, rule, netgroup surfaces consolidated

- **The CLI and web now share the parsers that shape checks, rules, and
  netgroups.** `store.ParseCheckTargets` (strict for `check add`, blank-skipping
  for the checks form), `store.ParsePrefixList`, `store.ClassifyNetgroups`
  (one-pass address classification), and `store.CheckIntervalLabel` are the
  one home both surfaces parse through; the rule label helpers (action, source,
  cooldown, cap, approval) now live in `internal/reactive`, so `rule list`
  and the /rules page render through the same code and cannot drift again. The
  deliberate unify of the CLI/web parsers is recorded here as the correction of
  the earlier "store remains the shared validator" framing.

## 2026-08-31 — checks page netgroup/recency; read-only rules page

- **The checks page shows the location and recency signals the feature is
  about.** Each target now renders the netgroup its probed address belongs to
  as a small chip next to the status chip — the registered group name (ok) or
  `away` (dim) — with no badge for an http URL target, whose liveness is not a
  location verdict. Beside it, a dim meta line reads how long ago the status
  last changed (`changed 3h ago`, `humanAge` of `check_state.LastChange`) or
  `unobserved` when the sweep has never seen the target. The classification
  reuses the engine's label semantics: a miss reads `away`, the same word the
  engine stamps on the transition event, so the page and the events it
  describes agree.
- **New /rules page** reads every rule with the action and guardrails exactly
  as `rule list` prints them (shell argv, tofu-plan|apply workspace, playbook
  source playbook (limit), notify; cooldown, daily cap, approval) and the CEL
  expression verbatim. `GET /rules` joins the post-v1 read surfaces with a
  Rules nav entry. The web now *reads* rules; mutation stays CLI
  ([design/check-triggered-runs.md](design/check-triggered-runs.md) §9's
  "rules stay CLI" is about the builder, not browsing).

## 2026-08-31 — dashboard checks card, job provenance, events filter

- **The dashboard gains a probe-estate card.** After the sources card, a
  Checks card shows the registered total, how many targets the sweep currently
  observes up (one aggregate count over `check_state`, never a per-check state
  loop), and the attention accent when a check is disabled — DOWN is normal for
  an away host, so it is not the signal.
- **Job detail links a rule-triggered run to its cause.** A job whose runbook
  is `rule:<name>` renders "fired by rule <name>" linking to `/rules` and,
  when the row is on record, the event that raised it (event #N, source, alert
  name, received time) linking to its trace. A manually enqueued job carrying
  a rule runbook shows just the rule line.
- **The events page filters by source.** A GET `?source=` filter narrows the
  received-alerts list to one pipeline (alertmanager, healthchecks, generic,
  drift, check); the form preserves the chosen source, so a filtered view is a
  linkable URL.

## 2026-08-31 — http check dial-time guard

- **The http probe refuses the addresses a check must not reach.** Its HTTP
  client dials through a `net.Dialer.Control` hook (the push transport's T16
  dial-time SSRF check, scoped) that refuses loopback, link-local unicast —
  covering the cloud metadata address `169.254.169.254` — unspecified, and
  multicast at the connected IP, while keeping the RFC1918/ULA LAN targets a
  check exists for. A check against a refused address observes down.
  [security/threat-model.md](security/threat-model.md) T36 + the checks section
  of [architecture/reactive.md](architecture/reactive.md). The probe's GET path
  is tested both ways: the guarded client refuses a loopback target through the
  sweep, and an internal test exercises the successful status check with an
  injected client.

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

- **The web surface gains netgroups and checks pages.** `/netgroups` and
  `/checks` render their rows — checks with each target's last observed
  status from `check_state` — and carry operator-scope writes (add/rm,
  add/enable/disable/rm), plain operator scope like schedules: no reauth, no
  confirm_token. The read API adds `GET /api/v1/netgroups` and
  `GET /api/v1/checks` (wire kinds `netgroup.list`, `check.list`), readable
  by any authed token. The check-add flow exercises the tcp/http kinds; kind
  `ping` still refuses on a host that cannot actually send ICMP.
- **The web write handlers reimplement the CLI's parsing instead of calling
  it** — the comma-split + `ParsePrefix` list for netgroups
  (`cmd/exocomp/netgroup.go`) and the name=spec target split for checks
  (`cmd/exocomp/check.go`) — so a row added from the browser is held to
  exactly the same rules as one added from the CLI; the store remains the
  shared validator.
- **Rules stay CLI.** [design/check-triggered-runs.md](design/check-triggered-runs.md)
  §9 drops the in-browser rule-builder promise and the design's Status is now
  the shipped-subset summary; `architecture/reactive.md` gains a Checks
  subsection beside Drift and the `playbook` rule action, and
  `operations/reactive.md` gains the netgroup/check verbs.

## 2026-08-30 — check-triggered runs engine

- **The check engine lands: named probe sets become reactive events.** `check
  add/list/enable/disable/rm` (wire kind `check.list`) manages `check_row`
  rows; the daemon's `SweepChecks`, called on the same `--sweep-interval` tick
  as the drift sweep, probes every enabled check's targets on their interval and
  emits one event into the existing pipeline per declared transition. Kinds:
  `tcp` dials `ip:port` (the address is the location), `ping` runs a pinned
  `ping -c1 -W2` subprocess (exit code only), `http` reads the status line and
  closes the body without reading it (AGENTS.md 4). A transition declares only
  after N=3 consecutive observations (hysteresis) in both directions, so a
  marginal-wifi host cannot flap a pending approval. A target's event carries
  `labels["netgroup"]` — the group containing the probed address, or `away` —
  and no group for an http URL target (its liveness is not a location verdict).
  Implemented per [design/check-triggered-runs.md §5](design/check-triggered-runs.md).
- **`check_state` schema refinement vs the design doc §4.2.** There is no
  `unknown` status: a missing row IS the unobserved cold-start state, and the
  first probe seeds a row (`status=result, consecutive=1, emitted=result`)
  **without emitting**. `emitted` is seeded to the result, never `''`, so a
  daemon restart never spurious-fires every up target as a fresh down→up
  transition; a target home at start is seeded up and its join is missed once,
  the documented fail-safe. `consecutive` carries the hysteresis counter and
  `emitted` the last-declared status, so a steady state never emits.
- **Per-target cooldown is check-only, not sweep-source-wide (design §5.5
  narrowed).** `guardCooldown` reads the fire ledger per dedup key
  (`check:<check>:<target>`, a bounded daemon-controlled set) only for the
  `check` source; drift stays per-rule because its keys are per-job and per-key
  cooldown would bypass the throttle, and webhook keys are fingerprint-based
  where a varying body would get a fresh budget. The daily cap stays per-rule
  for every source: a second target joining inside the rule's window is
  throttled, and its event row still records the match as suppressed.
- **A check's effective interval is floored at the sweep tick.** The in-memory
  due map arms the next probe one interval after the last on the engine clock,
  but the sweep itself runs on the 30s default `--sweep-interval`; a check
  interval shorter than the tick cannot probe more often than it.
- **`check_state` continuity, v1 has no target-edit verb.** A check's targets
  are fixed at add; state rows persist for the check's life and cascade away
  with it on delete, so the sweep never re-seeds (and never re-misses) a target
  that was only being edited.

## 2026-08-30 — netgroups and peer stamping

- **Netgroups land as a store table, CLI verbs, and webhook peer stamping** —
  phase 1 of [design/check-triggered-runs.md](design/check-triggered-runs.md).
  `netgroup` rows name a comma-separated CIDR set, managed by `exocomp
  netgroup add/list/rm` (wire kind `netgroup.list`); prefix validity is
  enforced at write time, so a row that reads parses. Every webhook the
  ingress records now carries `labels["peer_ip"]` (the peer address, unmapped)
  and `labels["peer_netgroup"]` — the registered group whose prefix most
  specifically contains the peer, or `away` when none does — stamped before
  the pipeline evaluates the event. The stamp is a network-path signal, not a
  location verdict: over tailnet the peer is the CGNAT `100.64.0.0/10`
  address, so a LAN netgroup marks an overlay-delivered hook away, the
  fail-safe direction for a "refuse from outside" rule.
- **Reconciliation vs the design doc.** §8.1's "first registered group" is
  implemented as *longest prefix, then name ascending*. Code wins: "first"
  would silently change with insertion order on a later `netgroup add`, while
  a most-specific match is what an operator reading the prefixes expects, and
  the name tie-break keeps the verdict deterministic.

## 2026-08-30 — governed playbook rule action

- **A rule can now launch a governed ansible run instead of only a fixed shell
  argv.** New `ActionKind "playbook"` carries the registered source, the
  playbook path, and the ansible passthrough (`limit`, `tags`, `check`,
  `diff`). A new `reactive.PlaybookBuilder` seam — interface in reactive,
  service-backed impl wired by the daemon — resolves the source checkout and
  confines the playbook to it; the engine still owns the queue, the gate, the
  approval, and the identity fields (Runbook/Trigger/DedupKey/ConcurrencyKey/
  Timeout). Implemented per [design/check-triggered-runs.md §7](design/check-triggered-runs.md).
- **`--limit` is required and fixed at add time** (default-deny): an
  un-limited rule would run the playbook against every host the inventory
  names. The CLI flag for the playbook's registered source is `--repo-source`,
  because `rule add` already binds `--source` to the event-source filter; the
  store field stays `source`.
- **A rule playbook is a plain, vault-less, inventory-discovery play for v1:**
  no run-secrets or vault binding on rules. A hooked source is refused at add
  time and at fire time, covering both hook sources (the checkout's own
  `.exocomp/hooks.yml` and the data-dir override), because a rule never
  composes the lifecycle workflow `exocomp run` owns.

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

## 2026-08-26 — dashboard redesigned as at-a-glance summary cards

- **The dashboard's Pending approvals, Queue, Workspaces, Sources, and Workflows sections
  are summary cards, not embedded tables.** Each card is itself the link to its full view
  (`/approvals`, `/jobs`, `/workspaces`, `/sources`, `/workflows`); no full list renders on
  the dashboard anymore, and the Queue card counts the active queue only — settled job
  history never reaches the page. Pending approvals leads the grid and carries the
  wait/amber attention accent with a one-line subline naming the soonest-to-expire item;
  workspace drift and source attention use the same accent. Workflows in flight hides
  entirely at zero rather than showing a permanent "None". The Schedules card is unchanged.
- New `store.CountFailedJobsSince` backs the Queue card's "N failed recently" subline with a
  single COUNT query instead of a loaded-and-len'd list.

## 2026-08-26 — Run again resubmits with tweaks; run options are toggles

- **"Run again" is a resubmit modal, not a link to the source launcher.** It shows for
  any terminal job carrying an argv (succeeded included, ad-hoc included — it no longer
  needs a source), prefilled from the finished run: Check/Diff/Privileged toggles, editable
  limit/tags, read-only source/command/inventory/extra-vars. A plain submit repeats the run.
- **`POST /jobs/{id}/rerun`** clones the original row's `JobArgs` into a `queue.Spec`,
  applies the overrides on the cloned argv (add/remove `--check`/`--diff`, replace
  `--limit`/`--tags`, set `Privileged`), and enqueues a fresh job (trigger `rerun`) through
  `service.Reenqueue` — the same actor/privileged attribution tail the run verbs use. Only
  names travel (VaultProject/RunSecretBinding/Inventory/Source), never secret values.
- **Security**: the rerun's operator-scope gate is `writeGuard`, run before the privileged
  flag is read, so a readonly token's rerun (privileged or not) is 403 and enqueues nothing.
- **Run-option checkboxes are toggle switches.** A shared `toggle` component (a CSS switch
  over a real `role="switch"` checkbox — still the form field and JS-off fallback) replaces
  the source launcher's checkboxes and drives the rerun modal.

## 2026-08-26 — jobs carry and display their source name

- **A job now names the registered source it ran from, not just its runbook type.**
  `Source` rides `JobArgs`/`queue.Spec` like `Inventory`/`RunnerName` — a name on the job
  row's `args_json`, no schema migration. Every source-scoped run path sets it: `playbook
  run --source`, `run`, `ansible`/`command --source`, `run-cli --source`, and the web
  source launcher; a hooked source's pre/post-run and gate steps inherit it too.
- **The `running` job event no longer dumps an absolute checkout path.** With a source it
  now reads `running <source>: <argv, relativized against Dir>`; without one but with a
  working directory it still relativizes; a job with neither is unchanged.
- **Display**: the web jobs list gets a Source column and job detail names it in its meta
  line (falling back to the old Dir→source reverse lookup for a pre-existing row); `exocomp
  jobs list`/`get` and the wire `job.list`/`job.get` JSON gain a `source` field; the TUI
  jobs table gets a SOURCE column.

## 2026-08-26 — UnifiedPush per-device push endpoints

- New v1.1 push path: a device registers its UnifiedPush distributor URL over
  `POST/GET/DELETE /api/v1/notify/endpoints` (operator write; register/delete scoped to the
  owning token). New `push_endpoint` store table + CRUD (migration 0028), `wire.PushEndpointView`
  (`notify.endpoint`), and a store-backed `notify.UnifiedPush` transport wired into the serve-path
  Notifier alongside ntfy/goalert.
- The endpoint URL is client-supplied and the daemon POSTs to it, so SSRF is guarded in two layers:
  registration rejects non-https/credentialed/private-resolving URLs (`400`), and the transport's
  HTTP client dials through a `net.Dialer.Control` hook that re-checks the actual connected IP —
  the layer that defeats DNS rebinding. Added to threat-model as T16.

## 2026-08-25 — per-run privileged (unsandboxed) tier option

- **A single run can now opt out of the job sandbox.** `--privileged` on the run verbs
  (`playbook run`, `run`, `ansible`/`command`, `run-cli`) and a "Run unsandboxed (privileged)"
  checkbox on a source's web launcher run one job at the `disabled` tier — no kernel policy,
  full host access — even when the daemon default is `enforcing`. It is a per-spec override
  layered on top of the process-global tier resolver, not a change to it: the marker rides the
  job row's typed `Args` blob (no schema migration), the queue threads it onto the
  `executor.Spec`, and the three sandboxed drivers honour it by running their plain inner driver
  instead of wrapping it in nono. A privileged run that also named a `--runner` is refused —
  caps need the sandbox — never run silently uncapped.
- **Gated, off by default, audited.** Secure-by-default (absent the flag nothing changes;
  the default stays `enforcing`). Operator-scoped only: the web and API write faces refuse a
  readonly or approve token by the same operator-scope guard every run passes; the local CLI
  operator is already trusted. Loud: the CLI prints an unsandboxed warning, the web control is
  a deliberately-warned non-default checkbox. Attributed: an enqueue-time ledger event names who
  asked, and the queue records the unsandboxed execution on every path.
  [security/threat-model.md](security/threat-model.md) documents the escape hatch and its gating;
  [operations/serve.md](operations/serve.md) documents the per-run override alongside the tier.

## 2026-08-25 — `GET /api/v1/events/summary`

- **Event funnel as one object.** Tallies every stored event by outcome —
  `{received, ignored, matched, suppressed, gated, fired, error}` — through a new
  `store.CountEventsByOutcome`, the same `event` table and `outcome` column `GET /events` and
  `event list` read, so the two can never disagree. All-time, matching `GET /events`' own
  unwindowed listing; readable by any authed token, same floor as the rest of the events reads.

## 2026-08-25 — breadcrumb navigation

- **Back-nav buttons replaced with breadcrumbs.** Source detail, token mint/rotate result,
  approval decided/expired, source git-key result, and the secrets/sources/tokens reauth
  notices now show a shared `breadcrumb` trail (`view_layout.templ`) above the page content
  instead of an inline "back to X" button.

## 2026-08-25 — `playbook run --source` honors the checkout's ansible.cfg

- **A `--source` playbook run now executes with the checkout as its working directory.**
  Before, the hookless `--source` branch rebased the playbook onto the checkout but left the
  run's `Dir` empty, so the sandboxed job fell back to its scratch dir and the checkout's
  `./ansible.cfg` (and its relative `inventory`, `roles_path`, `collections_path`) was never
  discovered — inventory came up empty and roles read as "not found". `service.PlaybookInput`
  now carries `Dir`/`DirReadOnly`, resolved from the same `sourceCheckoutRoot` helper the
  run-cli and ad-hoc paths use, so the CLI grants exactly what the lifecycle `exocomp run`
  play step already grants: read on a read-only tree, read+write on a writable one.
  [operations/playbook-run.md](operations/playbook-run.md) documents the new posture.
- **Ansible scratch is pinned off the (read-only) source tree.** The sandboxed ansible driver
  now also exports `ANSIBLE_SSH_CONTROL_PATH_DIR` (alongside the existing
  `ANSIBLE_LOCAL_TEMP`/`ANSIBLE_REMOTE_TMP`/`TMPDIR`) into the per-job granted tree, so a repo
  `ansible.cfg` with a relative `local_tmp` or `control_path_dir` cannot force a write onto a
  read-only checkout — the env vars win over `ansible.cfg`. The data-dir/keys guard is
  unchanged: the checkout grant never extends to the database, `host.key`, or `keyring.age`.
- **The web + API `--source` playbook paths now set `Dir` too.** The browser launcher
  (`handleSourceRunPlaybook`) and `POST /api/v1/playbooks/run` (`apiPlaybookRun`) mirrored the
  CLI's earlier gap: they set the source name but not the checkout working directory, so a
  source run enqueued over the web or API still fell back to the scratch dir and ignored the
  checkout's `ansible.cfg`. Both now resolve the checkout via `resolveSourceCheckout` and pass
  `Dir`/`DirReadOnly`, matching the CLI — so inventory and roles resolve on every face.

## 2026-08-25 — phone-companion API round 2

- **`GET /whoami` now carries `server: {version, hostname}`.** The daemon's build version
  (`internal/version`) and its host ride the identity reflection, so the Connect screen can
  render `exocompd <version> · <hostname>` without fishing the host out of the URL. Both
  fields are omitempty; a missing hostname degrades the line, never the request.
- **`GET`/`POST`/`DELETE /api/v1/mutes`** expose the notify-layer mute (`store.NotifyMute`)
  the CLI's `mute add/list/rm` already drives: the job still runs, only its announcement is
  held (distinct from a silence, which suppresses the rule). The write takes the scope ladder
  the phone's mute sheet offers — `alert`/`host`/`global` — and either a `for` duration or an
  absolute `expires_at`; writes are operator-scoped, the list readable by any authed token.
- **Structured `stale` + `provenance` on the approval.** `GET /approvals/{id}` now carries a
  `stale: {plan_serial, current_serial, moved_by_job, moved_via, moved_at}` block (present
  only when the workspace moved under the saved plan) and a `provenance: {origin, alert,
  rule, matched_at}` block (`origin` ∈ alert/manual/schedule). The same `stale` block rides
  the `409 stale_plan` decision error body under `error.stale`. Both are sourced from the
  store — a new `store.ApprovalStaleness` returns the movement structurally, and provenance
  reads the event-ledger row that named the job — never by parsing a rendered sentence, and
  both carry identifiers only, never a plan attribute value.
- **One-shot `POST /jobs/{id}/notify`.** Arms an attributed, durable request to page once when
  a job reaches a terminal state — a clean finish included — so the phone's "Notify me" works
  even if the app was killed. The web route records one event-ledger row and runs nothing; the
  reactive daemon's new `SweepJobNotifyRequests` raises the page, sharing `SweepJobOutcomes`'
  checkpoint so a job that also fails is never paged twice. An already-terminal job is a `409`;
  a repeat request is idempotent.

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
