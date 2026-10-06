# exocomp — design invariants & philosophy

The rules that survive any redesign. A change that violates one of these is wrong —
or this file moves with it in the same commit.

Authoritative references, read together: `internal/web/tokens.css` (token values),
`internal/web/app.css` (layout), [DESIGN.md](DESIGN.md) (component token spec), and
the repo-root `DESIGN.md` (machine tokens). Where a document and the shipped code
disagree, the code wins and the correction lands in `docs/log.md`.

## Philosophy

1. **Instrument panel, not SaaS dashboard.** One operator, a handful of hosts. The
   UI's job is to make state legible and decisions fast — never to reassure, never
   to market. No vanity metrics, no progress theatre.
2. **Healthy is quiet, failure is loud.** A nominal system renders almost nothing:
   quiet counts, an empty attention band. Signal color is spent only where a
   decision is needed — that is what keeps it legible.
3. **Signals lead to decisions.** Anything highlighted must end in a verb — a link
   or button that resolves it ("Open checks →", "dead-man none — set one"). A
   highlight with no action is decoration.
4. **State is a word plus a color, never color alone.** Every state chip pairs a
   dot with an uppercase mono keyword. Must survive greyscale.
5. **Reachability ≠ location.** A check's UP/DOWN/UNOBSERVED is state and chips; a
   netgroup (home/away) is a network-path fact and renders as prose meta. Two equal
   chips claim two states — that is a lie.
6. **Gaps are signals, not grey facts.** "dead-man none" is a wait state with a fix
   verb, not a neutral label. Empty states name the verb AND the trigger that
   changes them ("No jobs yet. They appear when a schedule, rule or manual run
   fires."); a bare "Nothing here" is a bug.
7. **Fail closed, refuse loudly.** Approvals auto-reject at TTL; a stale plan is a
   refusal, not a disabled button; destructive actions go through the confirm
   dialog (label carries "…": "Remove…").
8. **The queue is the only executor.** Every run states what it will do — the
   launch drawer's footer names the exact ask ("same as exocomp run home-lab
   deploy/site.yml"), and the submit button is the bare verb so a long
   collection path can never wrap it. Nothing in the UI executes directly.
9. **Read first, act second.** Detail pages lead with identity and facts;
   launch/mutation UIs are focused surfaces (dialog/drawer) opened by intent.
10. **Code is truth for concepts and routes; pixels retune freely.** Concepts and
    their states come from the implementation; markup/CSS/JS are always negotiable
    in service of the rules above.

## Hard rules (a violation is a review-blocking bug)

- **Token or bug**: every color, size, radius comes from `tokens.css`. No raw
  hex/px in templates.
- **No shadows, no gradients, no heros, no illustrations, no mascots, no
  spinners.** Depth is fill (the surface ramp); the only motion is the RUNNING
  breathe (reduced-motion-respecting).
- **Hairlines in exactly two places**: a wide table's header rule and inset state
  rings (`box-shadow: inset 0 0 0 1px <state-border>`).
- **accent-fill vs accent-text are not interchangeable** (fill fails AA as text).
  The focus ring is always amethyst, 3px, 2px offset, `:focus-visible` only —
  never removed, never recolored to match state.
- **ink-600 is never text** — dots, rules, dividers only.
- **tabular-nums on every digit; ligatures off on every mono run.** Numbers align
  always. Mono for anything a machine produced (values, keywords, logs,
  timestamps); sans for anything a human wrote. Every mono run sets
  `font-feature-settings: "liga" 0, "calt" 0`, so operators like `==` render as
  separate glyphs, never one fused shape.
- **Icons are inline stroke SVGs** — 24 viewBox, stroke-width 1.8, currentColor,
  round caps. Never emoji or emoji-renderable glyphs. Typographic marks (‹ ▾ ▸ ·)
  are fine inside text.
- **Copy**: commands render as `exocomp` (never `exo`). Buttons one word where
  possible; the trailing "…" is retired — a button that opens a confirm dialog
  is named plainly and the dialog is the safety. Findings-not-praise tone; no
  marketing language. Insecure/cleartext warnings read as plain English for a
  human, never internal vocabulary ("on the wire", "listener", "served over
  cleartext").
- **Row actions**: a tray row shows at most two verb glyphs — the actions an
  operator reaches for daily on that resource — then a chevron that opens a
  menu. Fewer than three actions total → glyphs only, no chevron. Menu items
  are always glyph + word; destructive items live last, in `--signal-fail`,
  with an "asks first" note. The glyph set is fixed per verb (the same shape
  means the same verb on every resource); every glyph and the chevron carry an
  aria-label. Views (Saved plans, Backend config, Bindings, Open) live on the
  resource's detail page, not as per-row buttons. The phone chevron opens a
  bottom sheet listing every action.
- **One button-height ladder**: 34px default, 30px in tray rows and card feet,
  44px for a form's single dominant verb (the `.actions` block: modal submits,
  the launch drawer, login), 48px floor on every verb and primary button on
  phone (≤719px). The one documented exception is a 36px compact-secondary
  class for a secondary shell action (e.g. Raw log).
- **A list card's zero-state sentence shows only at zero** (a non-zero count
  never contradicts it), and it names the verb that changes the state.
- **A11y gates**: 48px touch targets on the phone surface (44px rows on desktop),
  200% reflow, keyboard reachable everywhere (mouse-only control = bug), greyscale
  legible, visible focus ring, TalkBack live regions on the approval surface.
- **Both themes always.** Dark is native, light is a warm low-chroma remap — a
  palette change is a `tokens.css`-only edit, and every component ships in both.
- **JS-off baseline.** Server-rendered forms POST; dialogs degrade (`?launch=1`
  renders `<dialog open>`); enhancements (row-click, xselect, drawer) are additive.
- **One color for "never" — wait.** Never-seen / never-synced / never-backed-up is
  a gap, not a failure: `--signal-wait` everywhere (Sources, Backups, UNOBSERVED).
  Exception: a dead-man policy that makes a missing backup a hard fail — then it
  is `--signal-fail`, and the copy says so.
- **Header meta is the page's own fact.** The app-bar meta slot carries the page's
  own fact (host · uptime, `{n} checks`, `1 of 1 down`); the operator identity
  renders once per screen, in the powerline only.

## Tofu surfaces (v0.10)

The round that surfaced the job-log and workspace-state problems. Source:
`tofu/SPEC.md` in the v0.10 handoff. Four invariants, each a hard rule:

- **A log owns its scroll.** The log box ends above the powerline, follows the
  tail while streaming (FOLLOWING TAIL), and shows a PAUSED word + a Tail verb
  when the operator scrolls up. The page never scrolls to reach a log.
- **One apply per workspace.** Only the latest SAVED plan carries an apply
  affordance (a glyph); plan history is read-only, one lifecycle word per plan
  (SAVED · SUPERSEDED · CONSUMED). Drift is a finding (wait), never a failure.
- **Secrets never render in a paste block.** Config the operator copies
  references a secret by variable or name; reveal is a separate, logged, timed
  action beside the block; copy never includes the value.
- **Mutations preview their diff.** Apply and destroy open a confirm showing the
  structured change set with +N ~N −N counters; a stale plan renders REFUSED
  with "Plan again", never a disabled button.

## Responsive (v0.9)

The web surface serves every width with one template structure re-flown by the
stylesheet; phone is not a second design.

- **One breakpoint.** `@media (max-width: 719px)` is phone, and phone only.
  720–1099 keeps desktop layouts with the rail collapsed and `.config` at two
  columns; `max-width: 1440px` is the one wide-desktop tweak (the system-backups
  footer stacks below it). Any other width breakpoint is a bug until a SPEC says
  otherwise. Touch hints are `(pointer: coarse)`, never a width test.
- **Rows, not tables, on phone.** Every list row (jobs, queue, events) stacks
  into two lines — a primary line over a mono meta line — the whole row is one
  link, 44px minimum. The wide table's header row is `display: none` on phone
  but stays in the a11y tree. A value is never ellipsized on phone:
  `overflow-wrap: anywhere` before truncation.
- **Logs are pre by default.** On phone the log is `white-space: pre` in a
  300px scroll box with a 60px clock column; wrapping is the operator's opt-in
  toggle (`wrap`), never the default.
- **One phone shell bar.** Phone chrome is a single 56px bar — menu · mark ·
  page title · right meta — and the wordmark drops. The powerline foot is
  sticky at 36px; `main` carries 50px bottom padding.
- **Size-agnostic markup.** One template structure serves both sizes; the
  stylesheet re-flows it. Copy differs per size only where the SPEC names it.
- **Touch floor, documented exception.** The 48px floor (a11y) stays for verbs
  and primary buttons; a 36px compact-secondary class (e.g. Raw log) is the
  documented exception. The app bar's right meta slot stays visible on phone
  (clock · count · last 24h) — it is not hidden.

## Events + job output (v0.12)

The round that rebuilt the Events filter row and the job Output status. Source:
`events-job-output/SPEC.md` in the v0.11 handoff. Four invariants, each a hard
rule:

- **Filters: scope is a switch, state is a chip.** A dimension with exactly one
  value at a time (event source) is a segmented switch; a closed enum you filter
  by (event outcome) is a chip row in enum order, all positions always present,
  zero-count positions dim and unlinked. The active chip is the only accent
  fill on the row and is its own clear.
- **One status word per surface.** A live-updating status chip (the job log's
  pin) is rendered by the server in its initial state and moved by JS; never a
  second static copy of the same fact.
- **A count is a column.** Word and number are two cells (tabular, right
  aligned), never `word · N` in one run of text.
- **Constant verbs hold fixed slots from the right edge.** In any glyph cluster
  the always-present verbs own the right-most positions on every row; a
  conditional verb (apply) is prepended on the left and is absent — not
  disabled, not a placeholder — when it does not apply. A constant glyph never
  changes x between rows or states.

## Component vocabulary (one of each, everywhere)

chip · card · tray-table (44px radius-10 rows in a padded card) · quiet/danger
buttons (danger is a ring until confirmed) · banner · native dialog + confirm ·
step rail · log tail · powerline (signal segments) · grouped nav rail
(OPERATE / AUTOMATE / PROVISION / ACCESS + System foot).

Component token specs and shipped CSS: `internal/web/tokens.css`,
`internal/web/app.css`, [DESIGN.md](DESIGN.md).
