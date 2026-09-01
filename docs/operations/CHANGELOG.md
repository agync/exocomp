# Changelog

Software releases, features, and fixes. This is the project's release narrative,
kept separately from the knowledge-base evolution log ([docs/log.md](../log.md)),
which records only bundle-structural history and corrections of fact.

Newest first.
## 2026-09-01 — saved-plan 409, runbook display parity, tool gate

- **A saved-plan apply on an unadjudicated plan is a 409, not a 500.** The
  browser form's apply handler now maps the service's `PlanUnadjudicatedError`
  (which unwraps to `store.ErrStateConflict`) to a conflict response with its
  actionable message, matching the API face — reachable by planning an
  unpolicied workspace, adding an enabled policy, then applying the saved plan.
- **The job runbook is displayed consistently across surfaces.** The CLI and
  the remote CLI now strip a `rule:` prefix for display (like the web), while
  `--runbook` still filters on the exact stored value (documented); the wire
  `JobView.Runbook` stays raw.
- **The full gate requires the real tools again.** `requireTool` skips only
  under `-short` and fails the full `make check-race` when restic/age/git are
  absent — the fast commit gate stays hermetic, the merge gate re-asserts the
  toolchain.


## 2026-09-01 — approvals stream hardening

- **The approvals stream is auth-safe and robust.** The browser `/approvals/stream`
  re-resolves the session per poll (epoch + expiry stamp), so a rotated token or an
  expired session ends the stream instead of continuing for up to 12h — the same
  check every ordinary request makes. Concurrent streams are capped (shared funnel,
  both the browser and API routes) with a list-appropriate 2s poll interval, an idle
  heartbeat keeps the connection alive so an intermediary timeout no longer silently
  kills the live list (the client reconnects instead of closing), and a
  server-computed fingerprint of the rendered pending set closes the
  render-to-connect gap (no blind first-event skip). Streams end at server shutdown
  so `exocomp web` Ctrl-C no longer burns the full grace period.


## 2026-09-01 — destructive writes confirm and store validation flashes

- **The /secrets Delete form joins the shared confirm dialog.** It carried the hidden
  `confirmed=1` field but no `data-confirm` marker, so the dialog never
  intercepted it — every other operator-visible destructive form already had
  both. The six rm/revoke handlers (checks, netgroups, schedules, runners,
  tokens, secrets) now also enforce that hidden field server-side, matching
  policies and sources: a confirmed-less destructive POST reads 400.
- **Store-side operator-input refusals no longer read "internal error".**
  CreateCheck/CreateNetgroup/CreateSchedule return the new exported
  `store.ErrValidation` for name/kind/target-shape typos, and the checks, netgroups,and
  schedules add handlers flash the message instead of routing it to the 500 path.

  CreateSchedule also gains the ValidName gate the web add form already had,
  closing the undeletable-schedule hole (a name the `/schedules/{name}` route
  could never address).
- **The phone approval surface's global-kill block keeps its deliberate 26px top
  separation.** The inline style it replaced became a single-class `.mt26` that a
  later `.approve-block` rule would collapse to 14px; the compound
  `.approve-block.mt26` selector restores it.


## 2026-09-01 — workspace saved-plans modal

- **The workspace row's inline plan-id input becomes a Saved plans modal.** A
  runs-in-server workspace row now lists its saved plan artifacts in a modal, each
  posting the artifact id to the existing apply route (which already routes
  saved plans through the plan gate); a fresh Apply stays one post away. State-
  only rows — never planned by the daemon — get no control or modal.


## 2026-09-01 — inline styles consolidated into classes

- **The web templates carry no inline styles.** The 55 literal `style="..."` attributes
  (card top margins, inline action forms, the workspaces plan-id width, the phone kill-expiry
  field) move into named spacing/utility classes in `app.css`, each reproducing its value
  exactly. The `!important` override that fought the header-adjacent card margin is dropped
  with the inline margin — both selectors stay as plain `margin-top: 0`.


## 2026-09-01 — approvals list live-updates via SSE

- **The /approvals list now live-updates over SSE.** The page opens a browser-session stream (`GET /approvals/stream`, cookie-auth, the same `tailApprovals` the phone API already serves) and reloads when the pending set changes, so a desk or phone operator no longer decides on a stale list. The first `approvals` event is the initial snapshot (the state the page already rendered), so the script ignores it and reloads only on a subsequent membership/state change; the dashboard card and /events live-update stay out of scope.


## 2026-09-01 — list-surface refinements

- **The list surfaces name their next action.** Every "No X registered." empty state now says what changes it — "add one" where the web has a write surface (checks, netgroups, runners, schedules, sources), the CLI for rules, a workspace for state. The checks and netgroups surfaces gain a one-line hint that nothing fires until a rule matches, linking the read-only /rules page.
- **Schedule detail is keyboard-reachable.** Each schedule row gains a focusable Details button that opens its spec modal (the row's mouse-click affordance remains).
- **The three detail pages share the breadcrumb** — job, event trace, and workflow now trail back to their lists like approval/source/token already did.
- **The check-add modal grows targets.** An "Add target" control clones the target input (capped), so a 5th+ target no longer forces the CLI.
- **htmx is dropped.** The vendored, never-used `htmx.min.js` include and embed are removed; the docs' UI-stack references now name the vanilla JS the app actually uses.


## 2026-09-01 — dashboard glance and destructive confirms

- **The dashboard's Schedules card now summarises, not lists.** It shows the
  registered total with an "N schedules · M disabled" tally, carrying the
  warn accent when any schedule is disabled (a disabled schedule has silently
  stopped firing), mirroring the other glance cards; the card is the link to
  /schedules, which keeps its full table with cron, next/last fire, and last
  outcome. No table remains on the glance screen.
- **Destructive actions confirm before they fire.** Every Remove, Delete, and
  Revoke form across /checks, /netgroups, /schedules, /sources, /policies,
  /tokens, and /runners now carries a `data-confirm` message that opens a
  shared native-`<dialog>` confirm modal in the layout; Confirm re-submits the
  form (whose hidden `confirmed=1` field the write handlers already required),
  Cancel, the backdrop, or Esc drops it. The app's modal idiom — never
  `window.confirm` — now guards the destructive write controls too.


## 2026-09-01 — write-form errors surface via flash

- **A bad form input no longer renders "internal error".** The checks,
  netgroups, and schedules add handlers route every operator-input validation
  failure — interval/kind/target/expect for checks, prefix parse and duplicate
  names for netgroups, cron/argv/target/concurrency-key/jitter and duplicate
  names for schedules — through the existing `?flash=` redirect to the owning
  page instead of `s.fail`'s 500, so the operator sees the actual message and
  the form can be retried. Duplicate-name branches flip from a bare 409 text
  page to the same in-layout flash. Genuine store errors stay 500.


## 2026-09-01 — check-event traceability and events pagination

- **The check-triggered flow is now traceable in the web UI.** A `check` event names its check and target on the events page and its trace instead of reading "check" for every one; an event that fired links to the job it enqueued; a check's name and an address target's netgroup chip link to the check-sourced event view, as do netgroup names.
- **Events paginate** — a "Load more" link raises the 100-row cap up to 500, preserving the source filter.
- **A rule-runbook job displays without the `rule:` prefix** in the jobs list and detail meta (the "fired by rule" provenance line already named it).


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

Correction: three back-dated log entries (list-surface refinements, write-form errors surface via flash, check-event traceability) now carry their true 09-01 dates; newest-first order restored.


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
  ([design/check-triggered-runs.md](../design/check-triggered-runs.md) §9's
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
  [security/threat-model.md](../security/threat-model.md) T36 + the checks section
  of [architecture/reactive.md](../architecture/reactive.md). The probe's GET path
  is tested both ways: the guarded client refuses a loopback target through the
  sweep, and an internal test exercises the successful status check with an
  injected client.


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
- **Rules stay CLI.** [design/check-triggered-runs.md](../design/check-triggered-runs.md)
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
  Implemented per [design/check-triggered-runs.md §5](../design/check-triggered-runs.md).
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
  phase 1 of [design/check-triggered-runs.md](../design/check-triggered-runs.md).
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
  Timeout). Implemented per [design/check-triggered-runs.md §7](../design/check-triggered-runs.md).
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
  [security/threat-model.md](../security/threat-model.md) documents the escape hatch and its gating;
  [operations/serve.md](serve.md) documents the per-run override alongside the tier.


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
  [operations/playbook-run.md](playbook-run.md) documents the new posture.
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
  host. New [operations/pairing.md](pairing.md) documents the payload contract.
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
  views use, so one parser covers both. [operations/api.md](api.md) updated.
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


## 2026-08-25 — access-token and inventory hardening

- **Confused-deputy guard.** `resolveRunSecrets` refuses to inject a sealed value shaped
  like an exocomp access token (`exo_<id>_<secret>`) into a job's env, closing the path
  where an operator binds the daemon's own credential as a run-secret and turns a job into
  a deputy for the API. One choke point covers every job driver.
  [security/threat-model.md](../security/threat-model.md) D2 updated.
- **Threat model renumbered.** The internal E/S/R/D/B/U/AI finding-id families (gaps,
  out-of-order entries, orphan residual refs) are replaced by a single sequential T1-T29
  catalog in document reading order; residual bullets lead with a short descriptive phrase
  instead of a dangling id.
- **Empty `--inventory` defers to ansible.** `AnsibleArgv`/`AnsibleAdhocArgv` no longer add
  `--inventory` when none is given, so ansible discovers its own (the checkout's
  `ansible.cfg`, then its default hosts file) instead of being pinned to `localhost,`. The
  venv-interpreter force now applies only to an explicit `localhost,` inline inventory.
  [operations/adhoc-and-partial.md](adhoc-and-partial.md) and
  [operations/ansible-first-bootstrap.md](ansible-first-bootstrap.md) updated.
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
  [operations/sources.md](sources.md) notes the web UI names this "Workspaces"
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
  `hostpath`/`volpath` split is gone. [operations/sources.md](sources.md) and the
  [ADR 0010](../adr/0010-source-and-credential-model.md) addendum carry the model.
- **Runs are launched from a source's page.** Each source has a playbook/command launcher; the
  standalone run surface is retired. Reflected in [operations/serve.md](serve.md).

