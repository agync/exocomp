# Changelog

Software releases, features, and fixes. This is the project's release narrative,
kept separately from the knowledge-base evolution log ([docs/log.md](../log.md)),
which records only bundle-structural history and corrections of fact.

Newest first.

## 2026-09-11 — per-job ansible egress ports (G3)

- **A sandboxed ansible job can open a nonstandard inventory port.** The
  `enforcing` tier granted only TCP 22, so an inventory host answering on a
  high port (a lima guest's loopback forward) was denied at connect time. A
  job can now declare extra outbound ports with `playbook run --egress <port>`
  (repeatable, or comma-separated), `run --egress`, or a playbook rule's
  `rule add --action playbook --egress`; the API (`egress_ports`) and the
  source-detail launcher carry the same field. The grant is additive on top of
  port 22, validated (`1..65535`), and **empty by default**: a job or rule
  that declares nothing keeps exactly the port-22 posture. Ports are threaded
  through the whole run path — queue, job row, executor sandbox profile,
  remote playbook request, and web rerun — so no face silently drops the
  declaration, and the port is sorted/de-duplicated into the sandbox profile
  like the tofu egress grants.

## 2026-09-11 — the plan-diff view is readable (v0.11, §4.1)

- **The plan-detail diff renders three value shapes** (SPEC §4.1, the
  `plan-diff-readability` request): inline `old → new` for a short scalar
  change, two full-width `− / +` lines for a longer scalar, and an indented
  `<pre>` object diff with key-by-key `− / +` leaves for a map or list value.
  Server-set fields (`uid`, `resource_version`) render `[redacted ·
  server-set]` and never count as a change; secret-backed values stay
  `[redacted · secret]` on every surface. No `line-through` anywhere — old and
  new are carried by colour, tint, and the arrow, matching the phone approval.
- **The false diffs are gone.** The attribute classifier compared before/after
  as JSON strings, and Go map key order is not stable, so byte-identical maps
  read as changed (`id → id`, `metadata → metadata`). It now compares the
  decoded values with `reflect.DeepEqual` (order-independent) and flattens maps
  to leaf rows (`data.marker`, `metadata[0]`), so the row cell `N changed · M
  same` and the `N attributes unchanged · show` footer count the same list and
  always agree.
- **The literal `continue` text is gone.** The template rendered a Go
  `continue` in a loop body as text; it now loops the changed set only.
- **Each attribute is its own grid row** (`150px minmax(0,1fr)`), so a wrapped
  value never pushes its key out of column; the `show`/`hide` disclosure lists
  the unchanged attributes (objects as `1 block · N keys · unchanged`), and a
  `raw` drawer opens the minified before/after pair.
- **NO-OP resources render `unchanged`** on the plan-detail page (previously
  dropped); the phone and `/api/v1` still filter them out.
- **The apply/destroy confirm reuses the same grid**, capped at three
  attribute rows per resource then `N more →`.

## 2026-09-10 — the plan-detail diff renders from a captured show-json (v0.10, fix)

- **The plan-detail page and the approval surfaces show the diff again.**
  `/workspaces/{ws}/plans/{id}`, the approval plan view, and
  `/api/v1/.../plan` were feeding the saved plan's sealed **binary** to the
  classifier, which reads `tofu show -json` (`resource_changes`), so every
  plan read "nothing to diff". A saved plan is the binary tofu wrote, not a
  show-json document — and re-running `tofu show` at render time cannot be
  hermetic, because it needs the run's private `TF_DATA_DIR` (where the run
  installed providers), which is gone after the run.
- **The executor now captures `tofu show -json` at plan time**, while the run
  env is live, and writes it beside the plan file as `<plan>.show.json`. The
  sealer records it as a second artifact on the same job under a new
  `tofu-plan-show` kind, sealed with the same envelope; the plan artifact's
  digest stays the plan's. The web renders the diff from the show-json
  artifact and never from the plan binary, through the same `classifyPlan`
  redaction the approval phone and `/api/v1` use. A plan sealed before the
  capture existed (or an operator's own plan file) falls back to the old feed.

## 2026-09-10 — plan detail page and the CONSUMED lifecycle (v0.10, part 3)

- **`/workspaces/{ws}/plans/{id}` is live** (SPEC §4): the tray's diff/play
  deep-links now render the plan-detail page instead of 404ing. One card with
  the lifecycle chips (SAVED · LATEST, DRIFT · FINDING when the plan job
  recorded drift), `against serial N · recorded <time>`, the `+N ~N −N`
  verdict, and a structured diff parsed from `tofu show -json` through the
  SAME classifier the phone approval and `/api/v1` use — so a value one
  surface redacts is redacted on every surface. Destroys render first in their
  own fail block; each resource row shows the action keyword with its signal,
  the address, and `N changed · M same`, expanding to the old→new strike of
  the moved attributes with the unchanged remainder collapsed. The footer verb
  `Apply plan #id →` and the header's apply glyph open the confirm.
- **The apply confirm is a native `<dialog>`** that repeats the diff and asks:
  kicker `APPLY PLAN #id · WORKSPACE X`, `Change N resource(s) on <ws>`, a
  four-row fact grid (Summary · Against · Policy · Runner), and one primary —
  `Queue apply — ws @ plan #id`, `Queue apply — awaits approval` for a
  require-approval verdict, or `Plan again` for a stale plan. A deny verdict
  shows `DENIED · <policy>` with Cancel only, never a primary. The stale
  refusal is copy (`REFUSED · Tofu state moved to serial N`), never a disabled
  button (invariant 7). `?launch=1` renders the dialog open with JS off; the
  same dialog doubles as the destroy confirm (`?destroy=1`, kicker DESTROY,
  −N counters, danger ring).
- **CONSUMED is a real lifecycle word.** An apply that re-recorded its sealed
  plan (the CLI's same-SHA re-record) now dedupes by SHA256, so a second apply
  of the same plan never piles up a tray row and "N plans" reads honestly; the
  tray and plan page render `CONSUMED #job` with the applying job as a link,
  and the one apply affordance stays on the newest unconsumed plan.
- **The plan diff API grew `before`.** The `/api/v1` plan projection now
  carries each attribute's pre-apply value (`before`), empty for a created
  attribute, so the phone can render the same old→new strike.

## 2026-09-10 — workspace detail: plans tray and reference backend block (v0.10, part 2)

- **The workspace detail page's Saved plans list is now a read-only
  tray-table** (SPEC §2.2): one row per plan with PLAN / SERIAL / SEALED /
  CHANGES · VERDICT / LIFECYCLE / ACTIONS columns. LIFECYCLE reads SAVED for
  the newest plan and SUPERSEDED for every older one (CONSUMED lands with the
  plan-detail lane); CHANGES reads the plan outcome's `+N ~N −N` counters,
  resource names, and drift note; the policy verdict the plan job recorded
  (deny · <policy>, require-approval · pending, or no policy matched) rides in
  the same cell. A SAVED plan whose serial the Tofu state has moved past reads
  `stale · state at serial N` and loses its apply affordance.
- **One apply glyph, deep-linked.** The old per-row "Apply plan" button and the
  `plan=` hidden field are gone; the single play glyph sits on the latest SAVED
  non-stale row and links to `/workspaces/{ws}/plans/{id}`, the plan-detail
  page that will own the apply confirm. Each row keeps a diff glyph to the same
  route.
- **The Backend card emits a reference, never the bearer** (SPEC §2.4): the
  pasted block carries the address and lock plumbing only, and the init line
  `tofu init -backend-config=<(exocomp workspace backend <ws> --config)` fills
  the value at init time. A reveal glyph shows the derived bearer beside the
  block for 30 s and logs the reveal to the web server's own log; copy copies
  the block as printed, never the value. Reveal is operator-scoped.
- **New web surface tests** pin the single-apply rule (mutation check: a second
  row renders no apply glyph), the LIFECYCLE words, the CHANGES/VERDICT cells
  from append-only job events, the reveal endpoint's operation-only answer, and
  the leak check (the pasted block carries no bearer).

- **The workspaces list grew the T7 columns.** Each row is now the two-line
  64px density: NAME · POSTURE (runs-in-server with its newest job, or
  state-only with "external tofu") · DRIFT (NONE / PRESENT / LOCKED chip) ·
  TOFU STATE (serial · serials · size with the ENCRYPTED keyword and its writer
  meta) · ACTIONS. The row keeps the Plan glyph; the Apply glyph appears only
  while a SAVED plan exists (invariant: one apply per workspace) and a
  state-only row's Backend glyph links the detail page.
- **A LOCKED row is actionable from the list.** The TOFU STATE meta names the
  lock holder and since when, ending in a force-unlock button — the same
  audited verb as `exocomp workspace unlock`: a live job's lock is refused, the
  break is recorded on the job the lock named (or the workspace's newest), and
  the row returns to its ordinary state.
- **A drifted row links its open approval.** The DRIFT chip reads PRESENT with
  an approval link to the pending decision, so a workspace waiting on a human
  is one click from the ask.

## 2026-09-09 — the job log owns its scroll (v0.10, part 1)

- **The job-detail page is now a no-page-scroll flex column** (scoped to
  `<main class="page-job">`): the log box ends above the powerline, owns its
  own scroll, and the page never scrolls to reach it. The previous `max-height:
  60vh` box let a long log's tail fall below the viewport fold with the page
  itself as the scroller.
- **A streaming pin.** While a job runs, the Output bar shows a FOLLOWING TAIL
  pill; scrolling up pauses it (PAUSED · scrolled up) and a floating Tail +N
  button returns to the bottom. End of run shows ENDED · nothing to follow.
- **Header verbs are glyphs (T6).** Raw log and Rerun in the app bar are now
  34px stroke-SVG buttons (document · rotate), right-aligned, with aria-labels
  carrying the word.
- **New invariants** (docs/design/INVARIANTS.md "Tofu surfaces v0.10"):
  a log owns its scroll; one apply per workspace; secrets never render in a
  paste block; mutations preview their diff.

## 2026-09-09 — workspaces can resolve tofu providers offline

- **`exocomp workspace offline <name> on|off`** flips a workspace's
  offline-provider posture: on makes every tofu run resolve providers from the
  local filesystem mirror alone, with no `registry.opentofu.org` access — the
  amendment-8 offline guarantee, now a surface instead of an executor option
  nothing set. A provider missing from the cache fails the run rather than
  falling back to the network (fail closed). The posture is per-workspace,
  stored on the `workspace` row (migration 0035), and threaded into both the
  inline and daemon driver paths.
- **The offline path was already proven** (`TestTofuPlanRunsOfflineFromPluginCache`
  runs a network-denied init and plan from the cache); the gap was that no CLI
  or store surface could set `OfflineProviders`. Now it can, with an upgrade
  test pinning the column and a CLI test proving the posture is per-workspace.

## 2026-09-09 — OpenTofu state adoption works end to end

- **`exocomp workspace adopt <name>`** migrates a workspace's pre-existing local
  tofu state onto the HTTP backend. It runs one `tofu init -force-copy` (which
  implies `-migrate-state`) with an `unencrypted` read fallback in the
  `TF_ENCRYPTION` document, so an old local `terraform.tfstate` from a prior
  `backend "local"` is read, migrated, and drained (tofu keeps a `.backup`).
  This closes the gap that made the documented adoption runbook fail with
  "encountered unencrypted payload without unencrypted method configured".
- **The unencrypted fallback is one-shot and never the default.** A normal run's
  encryption document stays `aes_gcm`-only; the fallback exists exactly for the
  adoption run, and a normal plan over migrated state reads it cleanly. Tofu's
  own backend-change guard still protects a workspace that grows local state
  without an explicit adoption.
- **Verified against the real tofu binary** (executor e2e fixture): local state
  established with a local backend, adopted, then re-planned to no changes.

## 2026-09-09 — gates move to the pre-commit framework

- **The three bespoke hook scripts become pre-commit dispatchers.** `commit-msg`,
  `pre-commit`, and `pre-push` under `.agents/hooks/` are now one-line shims
  that hand off to `pre-commit run`; the rules live in a declarative
  `.pre-commit-config.yaml` instead of shell.
- **Standard hooks for hygiene.** `trailing-whitespace`, `end-of-file-fixer`,
  `check-yaml`, `check-json`, `check-merge-conflict`, and
  `check-added-large-files` from `pre-commit/pre-commit-hooks`. The TUI golden
  frames (`internal/tui/testdata/frames/`, `docs/design/reference/tui/`) and
  the store migrations are excluded — the frames are byte-pinned terminal
  widths, the migrations final at merge.
- **`conventional-pre-commit` validates the commit format.** Type + optional
  scope + subject; non-strict mode keeps `fixup!`/`squash!`/`amend!` and
  `Merge` subjects legal, and `revert` joins the allowed types for `git
  revert` subjects.
- **Repo-specific rules stay repo-local.** A `commit-msg-extras` hook keeps the
  title at 52 chars, forbids attribution trailers, and rejects AI-slop emoji —
  the checks `conventional-pre-commit` cannot express. A pygrep hook forbids
  real host paths (`/home/<user>/…`) in `docs/`.
- **`make check` and `make check-race` run as repo-local hooks** against the
  working tree (no more index-export clean-room: there should be no unstaged
  edits at commit time). `make check-hygiene` runs every pre-commit hook over
  all tracked files.

## 2026-09-07 — events: phone-responsive funnel, stacked rows, expand pane

- **The funnel reads as a three-column grid of monospace counts.** Each of the
  six closed outcomes is a 24px numeral over an 11px label, tinted per outcome
  (`in` ink-100, `matched` ink-200, `suppressed`/`gated` wait, `fired` accent
  when non-zero) with a 6px proportional bar; the "last 24h ·" note leaves the
  funnel for the app bar, which now reads `last 24h` in the meta slot.
- **Rows are two stacked lines instead of a seven-column table.** Line one is
  the alert with its outcome keyword and chevron; line two is `source · rule`
  with `{HH:MM} · {rel} ago` on the right, where `rel` is the age from the
  event's received time. The chevron still toggles ▸/▾ and `j`/`k`/`↵` still
  move and expand.
- **The reason leaves the row for the expanded pane.** It now answers "why"
  only once the operator opens a row: `reason · …` sits at the top of the pane,
  the evaluation trace comes first, and the normalized payload sits behind a
  `Show normalized payload →` button (`payload` is second on a phone). The
  keyboard hint stays in the markup and is hidden on touch by the coarse
  pointer media query.
- **The footer drops the enum sentence.** It reads `showing latest {n} of {t} ·
  retained 90d`; the closed-enum prose moves to the docs.


## 2026-09-07 — dashboard: counts tiles, queue card, insecure-banner verb

- **The COUNTS band is a 2×2 grid of whole-link tiles (SPEC §2.1).** Each tile
  is one `<a class="tile">` to its full view — label, mono 26/600 tabular value,
  and, only at zero, the verbatim CLI verb that changes it (`none waiting`,
  `exocomp ws add`, `exocomp schedule add`, `exocomp rule add`). The old
  `.counts-grid` card-list markup is gone.
- **The queue card reads as one section.** The card's "Queue" title is
  replaced by a `In the queue · {idle | N running, oldest queued …}` section
  label with an `all jobs →` link; the tray shows the live rows, the idle line
  (`○ QUEUE IDLE` + the copy sentence) when nothing is moving, and — under a
  `LAST {n} RUNS · {m} FAILED` rule row — the newest terminal jobs: FAILED and
  DEGRADED rows carry their reason and a `Rerun →` verb (a sibling link to
  `/jobs/{id}?rerun=1` that opens the rerun dialog, JS-off included), SUCCEEDED
  rows are quiet. The group is three rows, or two while the queue is live.
- **The insecure banner ends in a verb.** `▲ INSECURE HTTP` + one sentence +
  `Serve over TLS →` linking this host over https, replacing the two-line fact
  with a decision.
- **The `failed recently` subline is retired.** Its store COUNT
  (`CountFailedJobsSince`) is dropped with its test; the recent runs group is
  the one settled-history fact the glance carries.

## 2026-09-07 — netgroup form: real-time validation + multipart fix

- **The netgroup-add form validates on blur, not just on submit.** A name with
  spaces or a malformed CIDR shows its `▲` message the moment the operator
  leaves the field, and clears when corrected — the daemon stays authoritative
  on uniqueness, so submit still asks the server.
- **The browser form-fetch path no longer loses the typed values.** The JS
  submit POSTs a multipart `FormData` (a fetch body cannot carry urlencoded),
  but the write guard parsed only urlencoded forms, so a 422 re-render echoed
  empty fields and the dialog reset to an empty form after a refusal. The guard
  now parses multipart too, so a corrected re-submit keeps the values.

## 2026-09-07 — events: funnel, filter chips, tray with inline explain

- **The /events page leads with a last-24h funnel.** One card tallies every
  outcome in the window (`17 in · 11 ignored · … · 0 fired`) against a
  proportional bar of the closed enum; the counts come from a new windowed
  store query (`CountEventsByOutcomeSince`) that rides the existing
  `event_received_at` index — no migration, no second table.
- **Filter chips replace the source dropdown.** `all`, `checks`, `alerts`, and
  the six outcomes each re-run the page under `?filter=`, counting the rows in
  view; the active chip wears the accent tint, and the row of chips hides
  entirely at zero events (R7). The alerts split is a new `NotSource` clause on
  the event list filter ("everything but check").
- **The tray table's rows expand to a two-pane.** Every row is a button that
  opens the stored normalized payload beside the evaluation trace (the same
  PASS/STOP rendering the /events/{id} page uses, decoded from the row), with
  the reason column enriched per outcome: FIRED reads the live job
  (`job 4 · succeeded · exit 0`) and links to it, GATED names the approval and
  its TTL and links there, a suppressed silence or cooldown maps to the design
  copy, and ignored/matched are the fixed sentences. `j`/`k` move the row,
  Enter and `e` expand (`e` also brings the trace into view), Esc collapses.
- **The footer is honest about the ledger**: `showing latest {n} of {total}
  events · retained 90d · the enum is closed`. The /events/{id} trace page is
  unchanged.

## 2026-09-07 — netgroup add refusals render on the form

- **Netgroup-add refusals render on the form that owns the input, not a
  flash.** A refused name, CIDR, or duplicate re-renders the add dialog with
  the field's human sentence (▲ Names can't have spaces., ▲ 192.168.0/16 needs
  four octets — like 192.168.0.0/16., ▲ A netgroup called home already exists.)
  and its fail ring; a whole-submit failure reads on the status line
  (▲ Couldn't reach exocompd — nothing was saved. Retry). JS-off the same page
  returns HTTP 422 with the dialog open; JS swaps the form in place via fetch.
  Flash is success-only now ("home added (2 ranges)" — the flash cookie is
  ASCII-only, so the copy's · separator reads as a parenthetical), so a refusal
  can never wear the ok tint.
- **The field layout never moves.** Every control carries an always-present
  meta line (min-height 18px) that shows help text at rest and the refusal when
  invalid, so an error never shifts the geometry of the field or the dialog.
  aria-invalid and aria-describedby tie the control to its meta; the first
  invalid field receives focus, and after a refusal a field re-validates on
  blur and clears when right. The reusable fieldInput/formStatus components
  (view_form.templ) make the pattern mechanical for the other forms.

## 2026-09-06 — source delete names its cache removal

- **Deleting a source states the full scope of the action.** The source detail's
  Delete confirmation says the source's cached Python venv and ansible
  collections are removed with it, and `source rm` prints the same when it
  actually purged them (the shared-root guard still keeps a surviving source's
  caches). Deleting a source had already garbage-collected its dependency
  caches; this makes that explicit on both deletion surfaces.

## 2026-09-06 — jobs v2: run card, verdict band, reason lines

- **The job detail page is rebuilt around the verdict.** A run card (state chip,
  `exit {code}` / `exit {code} tolerated` verdict number, RUN CONFIGURATION tile
  grid) is followed by a verdict band on FAILED and DEGRADED — keyword line,
  per-host HOST/RESULT/RECAP table, and the rerun-tolerating / open-checks /
  rerun-limited verbs — then the output tail. The list becomes two-line rows with
  a one-fact reason line (`2 hosts unreachable`, `exit 4 · 2 hosts unreachable`)
  and never a host list.
- **The DEGRADED state is surfaced everywhere** it can land: web chips and verdict
  band (wait amber), the jobs list, the dashboard queue card's last-job line, and
  the TUI (`◐ DEGRADED`); the ntfy notify-request title is
  `degraded: {playbook} — {n} hosts unreachable`.
- **The rerun dialog and the source launch drawer gain a Tolerate unreachable
  hosts toggle** (pre-highlighted when the prior run failed on unreachable
  hosts); the detail-page flash is dropped in favour of a dashed `rerun of #N`
  chip, and the filter is placeholder-only.

## 2026-09-06 — degraded is a first-class job state

- **A tolerated unreachable run settles in the `degraded` state, not
  `succeeded` + a `partial` flag.** `--tolerate-unreachable` runs whose only
  non-success is offline hosts now end in `degraded` — a terminal "succeeded
  with warnings" that does not page. The job row keeps its true non-zero exit
  code; the CLI (`reportJobOutcome`, `remoteJobExit`) and a workflow step's
  ledger (`recordOutcome`) treat degraded as a success, so a tolerated offline
  host fails neither the caller nor the chain waiting on it. `partial` remains
  as the legacy marker, still written beside the state for readers that predate
  it. Schema: migration 0034 rebuilds `job` with the widened state CHECK and
  re-points every referencing table, preserving all rows.
- **A whole-inventory miss is not degraded.** A run whose recap shows every
  addressed host unreachable (the full inventory, or the full `--limit` set)
  falls through to `failed`: with nothing reached, nothing converged. Only a
  run that reached some host and succeeded there may degrade.
- **The state filters admit it.** `jobs list --state degraded`, the MCP
  `list_jobs` filter, and `GET /api/v1/jobs?state=degraded` all accept the new
  state, and the drift sweep treats a degraded `--check` run like a succeeded
  one — it opens (and a clean one never closes) a drift incident.

## 2026-09-06 — per-source ansible collections isolation

- **Ansible collections follow the venv model: per-source, hash-keyed,
  resolved at claim time** ([ADR 0015](../adr/0015-per-source-collections.md)).
  A source's collections live in `cache/collections/<source>/<requirements-hash>`,
  provisioned by `ansible-galaxy` as a subprocess of the claimed job when the
  hash changes (a matching ready marker is a no-op); the shared
  `cache/collections` dir is demoted to a legacy fallback for collections no
  source declares.
- **`playbook vendor` is now source-keyed**, resolving the source's
  `.exocomp/requirements.yml` (else repo-root `requirements.yml`) into the
  source's hash-keyed dir instead of the flat shared cache.
- **Collections gain lifecycle GC:** deleting a source removes its
  `cache/collections/<source>` and `cache/python/<source>` (only when no other
  registered source resolves to the same root), and a requirements-hash change
  prunes that source's old hashes with the venv keep-N + since policy.

## 2026-09-06 — job log: search + view raw in a new tab

- **The Live tail gained a search filter.** A "Search log" input above the log
  box filters the event lines in place (case-insensitive substring); clearing it
  restores the full tail, and lines the SSE stream appends are filtered to the
  same query live.
- **An icon-only "view raw log" button** (the open-in-new-tab glyph) in the Live
  tail header opens `GET /jobs/{id}/raw` in a new tab: the job's full event log
  as `text/plain`, one already-redacted message per line — the raw output to
  copy into a terminal or log viewer, minus timestamps and level chrome.

## 2026-09-06 — path sources are never "never-synced"

- **A path source is no longer a never-synced wait state.** It has no sync verb
  (`source sync` refuses a non-git source — the tree is already on the box and
  read directly), so `NeverSynced()` now reports false for a path source. The
  launch drawer's playbook select enables as soon as discovery finds playbooks,
  fixing the contradiction where the Playbooks card listed playbooks while the
  launcher stayed disabled with "none discovered yet" (the F14 gate, which
  reasoned "never-synced = no trusted tree" for git sources, had been applied to
  path sources too).
- **The source detail's Last-sync fact reads `n/a (path source)`** for a path
  source instead of "never — sync to discover playbooks", and path sources drop
  the NEVER chip and attention row in the list and the dashboard never-synced
  count. The never-synced wait state is now git-only, where the sync verb is
  real.
- **A disabled launcher select (no playbooks found) names the reason** via the
  same note as the Playbooks card: a never-synced git source says to sync; a
  synced source with an unusual layout says to give a path relative to the
  checkout.

## 2026-09-02 — v0.6 row-actions design (glyphs + chevron menu)

- **A shared row-actions partial** renders a tray row's up-to-two verb glyphs
  then a chevron menu, with a fixed per-verb inline-SVG glyph set (plan, apply,
  enable, disable, open, rotate, remove, bindings), aria-labels on every
  control, a destructive-last menu with the "asks first" note, and the phone
  48px floor. Views (Saved plans, Backend config, Bindings, Open) moved off the
  rows onto the resource pages.
- **Secrets and Workspaces** wire to the partial: secrets Rotate opens a reauth
  dialog (value/generate/confirm-token) and Remove is a confirmed glyph;
  workspaces gain a detail page (Saved plans + Backend config live there, the
  row opens it and keeps Plan/Apply glyphs) — closing R8.
- **Schedules, Checks cards, and Netgroups** wire to the partial: Disable/Enable
  glyph + Open/Remove menu, and a single Remove glyph for netgroups. "Run now"
  stays deferred — a schedule-fire route does not exist yet.
- **The trailing "…" is retired** from buttons and copy (the prefix
  truncation marker and select sentinels stay).
- INVARIANTS and the DESIGN.md component spec record the row-actions rule, the
  retired ellipsis, and the per-verb glyph set.

## 2026-09-02 — design-review v0.5 lane C (footnote, NITs, invariants)

- **The source sync note is a footnote, not an inline button-row text** (F4):
  a never-synced source's detail page carries
  `exocomp source sync <name>` under the identity card, like the Sources list
  footnote — not jammed beside the Launch verb.
- **`never fired` reads as wait** on schedules (R11), matching Sources'
  never-synced and the powerline dead-man — one colour for never.
- **The version string stops repeating itself** (R12): a dev build reads
  "dev", not "dev (dev) built dev" — a segment equal to the version is
  omitted; a real stamp keeps the full form.
- **INVARIANTS additions**: the row-action stance, the one button-height
  ladder, the zero-only list-card sentence, and the human insecure-warning
  copy rule. R9 (COUNTS zero-state) documented as zero-only (already the
  behaviour); R13 (light accent) verified as a fill-vs-tint reading, no token
  mismatch — both noted to the designer.

## 2026-09-02 — design-review v0.5 lane B (MED consistency)

- **Tray rows have no zebra** (R3): the alternating stripe is gone, so the dark
  theme's static captures show plain rows with hover only. Light was already
  flat.
- **One button-height ladder** (R4): 34px default, 30px in tray rows and card
  feet, 44px for the `.actions` block (modal submits, the launch drawer, login
  Sign in), and every button reaches the 48px phone floor below 560px.
- **One left edge on list cards** (R5): the kicker and hint/body lines align
  with the title and table at the content edge — the title no longer floats
  right of them. The system cards gained kickers ("Control-plane health",
  "Backup freshness") so they share the alignment.
- **Events filter only when there is something to filter** (R7): with zero
  events and no active source the filter row is hidden; an active filter keeps
  it and shows a filtered-empty line. The select applies on change
  (enhancement), the Filter button stays for JS-off.
- **Sources and Workspaces list cards gained kickers** (R10): "Playbook
  origins" and "Remote state", so every list card has one.

## 2026-09-02 — design-review v0.5 lane A (HIGH + still-open)

- **Danger buttons are a ring, not a fill** on the source-detail page (R1):
  `Delete…` is transparent with the fail ring until the confirm dialog, matching
  the tray-row and check-card danger buttons. The filled danger stays for the
  confirm dialog's Confirm.
- **Check card actions moved to the card foot** (R2): the Disable/Remove…
  pair sits below the target grid so the header stays one row (name · kind ·
  chip · down-count) on phones; the pair keeps the 30px row scale, ring-only
  danger, and the phone's 48px touch floor.
- **Rules and Schedules drop the empty ENABLED column** when no row diverges
  (F12/W17), matching Sources.
- **System Save is control-width**, not a full-width accent bar (W11).
- The playbook select on a never-synced source was verified already-disabled
  (F14) — reported to the designer as fixed.

## 2026-09-02 — UX fix round (actions spacing, banner copy, mobile powerline)

- **Tray action buttons breathe.** The tray Actions column is a spaced flex row
  (8px gap) with enough min-width that the common pairs — Rotate…/Remove…,
  Plan/Apply — sit on one line instead of flush together or wrapping. Applies
  to every tray table (workspaces, secrets, checks, netgroups, schedules,
  policies, runners, tokens).
- **The insecure-warning copy reads like a person wrote it.** "This surface is
  served over cleartext. Tokens on the wire…" is now "This page is not secure.
  Anyone on this network could read your sign-in." — and the login warning says
  the same plainly.
- **Mobile powerline shows what needs a decision.** On phones the ok and dim
  segments hide, so the strip renders only the signal that needs a look
  ("dead-man none — set one") instead of a cut-off scroll. A fully healthy
  phone shows a quiet empty strip. The icon-button direction stays deferred.

## 2026-09-02 — v0.3 review R2: web findings

- **HIGH 4 · design invariants:** ink-600 is no longer text — the nav group
  labels, tray column headers, and log timestamps read ink-500 (the last AA
  text step); the crumb separator (a rule) stays ink-600. The `font:`
  shorthand on mono runs no longer resets `font-variant-numeric` — tabular-nums
  is re-asserted after it on `.mono/code/pre/.log` and the check-card header.
- **HIGH 5 · queue card fact:** the queue card no longer hardcodes
  "last job — none yet" — it shows the active queue only, and the dead
  `.queuemeta` rule is gone.
- **HIGH 6 · dead-man via the store:** the powerline dead-man reads
  `store.LastSucceededBackupTime` (the shared rule: succeeded runs only, always
  finished), so a failed or still-running newest run never masks an older good
  backup.
- **MEDIUM · check-card actions:** the header's quiet/danger buttons get the
  30px row scale and the phone's 48px touch floor.
- **MEDIUM · grid reflow:** the attention/counts/target grids stack to one
  column below 720px instead of clipping a min-width track.
- **MEDIUM · count cards:** a count card shows its zero-state verb only when
  the count is zero.
- **MEDIUM · launch drawer:** the Command tab's Timeout field is restored.
- **MEDIUM · workspace apply** now routes through the shared confirm dialog.
- **MEDIUM · schedules spec modal** gains a keyboard-reachable trigger — the
  schedule name opens the dialog.
- **MEDIUM · job tail:** aligned with the approvals tail — SSE headers are set
  after the cap check (a refused tail is a plain 503), the tail emits the idle
  keepalive heartbeat, and the page carries the stale-live-updates banner with
  the approvals reconnect semantics.
- **MEDIUM · stream cap reorder:** regression tests for BOTH tails pin the
  per-token-then-global charge with undo — a refused attach leaves no phantom
  per-token slot.

## 2026-09-02 — design-review NIT pass

- **W13 · one left edge:** the tray-table card titles align with their rows (the
  12px indent is gone).
- **W14 · check card actions:** the per-check Disable/Remove… buttons sit at the
  30px quiet/danger row scale, and Remove… is a ring, not a fill, until the
  confirm.
- **W16 · netgroups CIDRs** render space-separated with ` · ` (the comma form
  stays for the add form's accepted input).
- **W17 · empty ENABLED column:** the Sources ENABLED column disappears when no
  row diverges from the default.
- **W18 · list-card kickers:** the Jobs and Secrets cards gain kickers, so every
  list card has one.
- **W19 · one colour for never:** a backup target with no run yet renders its
  NEVER fact as wait (a gap, not a failure), matching Sources' NEVER and the
  powerline dead-man.

## 2026-09-02 — design-review MED pass (consistency)

- **W4 · ligatures off:** every mono run (`code`, `pre`, `.log`, `.mono`) turns
  ligatures and contextual alternates off, so `==` and `->` in CEL expressions
  and log tails render as their own glyphs.
- **W5 · empty states name verb and trigger:** approvals, jobs, events, and
  workspaces carry the "… and what makes them appear" copy, not a bare "no X".
- **W6 · no per-row hairlines:** the system page's control-plane and backups
  tables use the tray surface (rows float, no cell rules).
- **W7 · identity once:** the app bar's meta slot is the page's own fact
  (dashboard: `host · up 6d 04h`); the signed-in identity lives in the
  powerline only.
- **W8 · schedules row is the link:** the redundant inline Details button is
  gone; the row opens the spec modal.
- **W9 · actions column + chips:** tray action columns hold their quiet/danger
  pair on one row, and chips never wrap.
- **W10 · workspaces Configure** is the header's primary verb (the app-bar slot,
  generalized to arbitrary label text), not an orphan row above the card.
- **W11 · system Save** is a control-width button beside the field and the
  `exocomp serve --public-url` note renders once.
- **F12 extended:** Rules and Schedules hide the default ENABLED chip (the
  column header carries the default), matching Checks and Sources.

## 2026-09-02 — design-review HIGH pass (W1/W2/W3 + F4 remainder)

- **W1 · header primary verb:** the app-bar Add button is a 34px nowrap accent
  button (`.addbtn` no longer caps at 32px transparent and strips the fill); on
  phones the meta slot hides before the verb shrinks.
- **W2 · attention band truth:** the NEEDS ATTENTION band shares the rail
  badge's one source of truth — a check estate with targets down surfaces as a
  fail chip + sentence + decision link ("2 DOWN · 2 of 2 targets down · Open
  checks →"). All six attention cards render the chip + one sentence + one
  decision link shape, never a count tile; the never-synced sources sentence
  names the sources.
- **W3 · phone powerline:** below 640px the powerline is one horizontal row that
  scrolls instead of wrapping over the content; the identity segment hides (it
  is already in the header).
- **F4 remainder:** the source detail gains a PLAYBOOKS card; the drawer's
  Options fold now holds the inventory/limit/tags/secrets fields closed by
  default; the footer button is full-width with the queue note on its own line;
  `@ {ref}` is omitted when a source has no ref (drawer meta, queue button, and
  the Ref fact row); the playbook select is disabled on any never-synced source
  (not just an empty discovery); and the `exocomp source sync` note is spaced
  from the Launch verb.
- **W12/W15 fixes:** the never-synced detail fact actually renders wait and the
  check down-count actually renders fail — `.fail-text`/`.wait-text` now sit
  after `.meta`, and `.kv dd.wait-text` beats the `.kv dd` colour.

## 2026-09-01 — check-transition observation ledger

- **A check transition now carries the hysteresis ledger that produced it.**
  The sweep records each probe into a bounded per-target observation ring
  (check_state.recent_observations, migration 0033) and embeds the transition's
  tail — filtered to the transition status, `observed {status} · {i} of 3` —
  into the event row at emission (event.check_observations_json), so a
  historical event stays self-sufficient after the ring moves on. This is the
  phone Explain card's "WHY THIS COUNTED AS A TRANSITION" data (checks-round
  D4/17) and it closes the same gap on the web Explain page, which now renders
  the ledger, the hysteresis note, and the cold-start body anchored to
  `whoami.server.started_at`.
- **`EventView.check_observations`** (additive, omitempty): the values-free
  ledger for a check event; absent for alert and non-check events. The ledger
  never lands in `normalized_json` — it is projected from the event row, one
  copy.

## 2026-09-01 — phone API: approval countdown + daemon start time

- **`JobView.expires_at`** (RFC3339, omitempty) for an `awaiting_approval` job:
  the pending approval's fail-closed deadline, so a phone queue row renders the
  `expires {hh:mm:ss} · {mm:ss} left` countdown tail. The web API, MCP reads,
  and the CLI's local `jobs list/get --json` and gated `run-cli --json` all
  resolve it through one shared wire helper, so the bytes stay identical across
  every face. Terminal jobs omit it.
- **`whoami.server.started_at`** (RFC3339, omitempty): the daemon's start time,
  so the events screen labels its cold-start state exactly ("since daemon start
  {hh:mm}") instead of approximating it from the earliest event. It is taken
  from the server's injectable clock so a test clock keeps it deterministic.

## 2026-09-01 — design-handoff v0.3 realignment

Lands the web and TUI surfaces against the v0.3 design handoff (the canon
imported to `.agents/brain/design-handoff/` — a local-only working directory,
not tracked in this repo): the web realign findings F1–F15
and the TUI write round. `tokens.css` was already byte-identical to the
handoff, so the foundation was untouched.

- **Web chrome.** The shell owns the page title in a slim app bar (F15); the
  nav is grouped by verb — OPERATE / AUTOMATE / PROVISION / ACCESS — with
  actionable-state badges (down targets on Checks, never-synced sources and
  pending approvals) (F7). The powerline renders dot + word in each segment's
  signal color and links to where it is fixed (F10).
- **Web lists.** List tables float in a padded tray with quiet mono headers and
  no per-cell hairlines (F11); per-row actions demoted to 30px quiet/danger
  ring buttons, Sources rows navigate and their disable/delete live on the
  source page (F8); labeled Add buttons replace the bare glyph (F9); Checks
  and Sources hide the default ENABLED chip (F12).
- **Checks rebuild.** One card per check with a target grid on the tray (F1);
  reachability is the only chip and location reads as prose (F2); a never-seen
  target is a single dim UNOBSERVED chip with its cold-start reason (F3); the
  per-target clock right-aligns in the tile header (F13). Netgroups became a
  tab on /checks — the standalone page and its write redirects point there,
  the one route change (F7).
- **Sources.** The detail leads with identity and facts; the launcher moved
  into a right-anchored drawer opened by intent, JS-off via `?launch=1` (F4). A
  never-synced source reads as a wait state everywhere — NEVER chip and
  attention row in the list, "never — sync to discover playbooks" on detail
  (sync is a CLI verb) (F5); an unsynced source with no discovered playbooks
  gets a disabled select with the path promoted (F14).
- **Dashboard.** The glance screen ranks by signal — a NEEDS ATTENTION band of
  only the non-nominal cards, a wide QUEUE card, then quiet COUNTS whose empty
  states name the verb that changes them (F6).
- **TUI.** Colors aligned to the semantic table (rules color, selected-row
  reverse video), the seven empty states wired with the verbatim copy, and the
  footer reports the real dead-man backup age. The interactive write keys are
  wired against the one queue and the store: pause/mute with mandatory expiry,
  approve/reject through the CLI's reactive seam (enqueue-only, never drained;
  stale plans refuse, destroy plans type-confirm), cancel, schedule toggle,
  follow, trace, outcome, filter, help, g/G, and tab/S-tab over every screen.
  The `b`/`l`/`x` run-now keys stay reserved but unwired: their spec
  construction lives in the CLI, and forking it would split the queue-facing
  contract.

## 2026-09-01 — event views carry check-field projections

- **`EventView` gains three additive, `omitempty` fields the mobile companion
  consumes:** `name` (the human-facing event name), `status` (a check
  transition's direction as the sweep observed it — `firing`/`resolved`), and
  `netgroup` (a check event's netgroup classification — the group whose prefix
  covered the target, or `away` when none did, the same value `CheckTargetView`
  carries; absent for alert events and http URL targets). Existing rows and
  clients are unaffected — empty values stay absent from the JSON.
- **The web's `eventName` derivation moved into `internal/wire` as `wire.EventName`
  and is now the one implementation the web pages and the API render through**
  (`dashboard_data.go`'s local copy and `api.go`'s `alertName` are gone; web
  output is byte-identical). The TUI and CLI surfaces name an event by its
  matched rule, not this derivation. `NewEventView`/`NewEventList` populate the
  fields from the same `Normalized` JSON the web reads, error-tolerantly.
- The phone companion's wire model already declares optional
  `name`/`status`/`netgroup` on its `ReactiveEvent`, awaiting these fields.

## 2026-09-01 — saved-plan 409, runbook display parity, tool gate

- **A saved-plan apply on an unadjudicated plan is a 409, not a 500.** The
  browser form's apply handler now maps the service's `PlanUnadjudicatedError`
  (which unwraps to `store.ErrStateConflict`) to a conflict response with its
  actionable message, matching the API face — reachable by planning an
  unpolicied workspace, adding an enabled policy, then applying the saved plan.
- **The job runbook is displayed consistently across surfaces.** The CLI and
  the remote CLI lists print the raw stored runbook — the exact value
  `--runbook` filters on, `rule:` prefix included — so a value read off a
  list is pasteable back into a filter, while `jobs get` keeps both the
  stripped rule name and the raw line; the web job list and detail still
  strip the prefix for display, and the wire `JobView.Runbook` stays raw.
- **The full gate requires the real tools again.** `requireTool` skips only
  under `-short` and fails the full `make check-race` when restic/age/git are
  absent — the fast commit gate stays hermetic, the merge gate re-asserts the
  toolchain.


## 2026-09-01 — approvals stream hardening

- **The approvals stream is auth-safe and robust.** The browser `/approvals/stream`
  re-resolves the session per poll — the credential epoch is pinned at attach, so a
  rotated token (whose row stays Active) or an expired session ends the stream instead
  of continuing for up to 12h — the same check every ordinary request makes. Concurrent
  streams are capped (8 per process, 4 per identity: enough open tabs for a real
  operator while still leaving half the fleet for a second identity) in a shared funnel
  both the browser and API routes charge per-token before the global slot, so a readonly
  attach flood cannot push the fleet over the total transiently. The list-appropriate 2s
  poll interval, the idle heartbeat that keeps a quiet list alive through intermediary
  timeouts (the client reconnects instead of closing), and the server-computed
  fingerprint of the rendered pending set that closes the render-to-connect gap (no blind
  first-event skip) all stand. Job tails now share the same funnel — a per-identity cap,
  a per-poll session re-resolve, and reconnect-on-drop — and an over-cap stream is
  refused with a distinct `streams_over_cap` code (process exit 7) so a phone or
  `jobs follow --remote` never mistakes a saturated stream for the auth gate's
  `kdf_busy`. Streams end at server shutdown so `exocomp web` Ctrl-C no longer burns the
  full grace period.


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
