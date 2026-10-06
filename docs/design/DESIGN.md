---
# exocomp web UI — design system
# Machine-readable token spec for the server-rendered web surface. Source of
# truth: internal/web/tokens.css (palette) and internal/web/app.css (layout).
# Values are transcribed from the shipped CSS. Dark is the native theme; light
# is a full remap. The in-page toggle cycles system -> light -> dark; "system"
# follows prefers-color-scheme.

colors:
  # Two complete palettes. Every token in `dark` has a `light` counterpart with
  # the same role. Contrast is measured against the surface the token sits on:
  # dark ink/signals vs surface-tray #171C1E; light ink/signals vs panel #FFFDF9.
  dark:
    surface:
      void:    "#0A0C0D"   # page backplate (body, log, plan)
      rail:    "#0E1112"   # app bar / left nav
      card:    "#121618"   # card and table shell
      tray:    "#171C1E"   # raised tray, row hover
      row:     "#191F21"   # selected / active row (declared; unused in web)
      control: "#22282A"   # button, input, chip base
      hairline:       "rgba(255,255,255,.09)"   # visible rule
      hairline-faint: "rgba(255,255,255,.06)"   # inner cell divider
    ink:
      "100": "#ECEFF0"   # 14.6:1 · headings, values
      "200": "#D6DBDD"   # 11.7:1 · body
      "400": "#A6AFB2"   #  7.5:1 · secondary, labels
      "500": "#8A9296"   #  5.4:1 · timestamps, meta, dim (last AA text step)
      "600": "#6B7477"   #  3.6:1 · dots/rules only — NEVER text
    accent:              # amethyst, deliberately split fill vs text
      fill:   "#A435CE"  # solid buttons/fills only · white on it = 5.1:1
      text:   "#BE7BEB"  # accent as text/icon/link/focus ring (fill fails AA as text)
      on:     "#ffffff"  # text/icon on a solid accent fill (both themes)
      tint:   "rgba(164,53,206,.14)"    # tinted fill (nav-current, run chip)
      border: "rgba(190,123,235,.34)"   # 1px state ring
    signal:              # state color — text, 1px borders, 8px dots
      ok:       "#4FB86B"   # 7.9:1 · succeeded, fresh, in sync
      wait:     "#E3A008"   # 7.6:1 · awaiting approval, stale, drift
      fail:     "#F0645A"   # 5.4:1 · failed, timed out, interrupted
      dim-text: "#8A9296"   # queued, skipped, disabled (text)
      dim-dot:  "#6B7477"   # queued, skipped, disabled (dots only)
      ok-tint:    "rgba(79,184,107,.12)"
      ok-border:  "rgba(79,184,107,.28)"
      wait-tint:  "rgba(227,160,8,.12)"
      wait-border: "rgba(227,160,8,.30)"
      fail-tint:  "rgba(240,100,90,.12)"
      fail-border: "rgba(240,100,90,.30)"
  light:               # warm, low-chroma remap — lab notebook, not inverted console
    surface:
      void:    "#F5F2EC"
      rail:    "#EFEBE3"
      card:    "#FFFDF9"
      tray:    "#FFFDF9"
      row:     "#F1EDE5"
      control: "#EFEBE3"
      hairline:       "rgba(60,50,38,.16)"
      hairline-faint: "rgba(60,50,38,.12)"
    ink:
      "100": "#1D1A16"
      "200": "#3A352E"
      "400": "#6B655B"
      "500": "#6B655B"   # same as 400 in light
      "600": "rgba(60,50,38,.28)"
    accent:              # one accent carries text, buttons and rings — no second tint
      fill:   "#7A2FA6"  # 7.4:1
      text:   "#7A2FA6"  # same value as fill
      on:     "#ffffff"  # inherited from dark; not re-declared in light
      tint:   "rgba(122,47,166,.10)"
      border: "rgba(122,47,166,.34)"
    signal:
      ok:       "#16703C"   # 6.2:1
      wait:     "#7A5200"   # 6.5:1
      fail:     "#A82F22"   # 5.7:1
      dim-text: "#6B655B"
      dim-dot:  "rgba(60,50,38,.40)"
      ok-tint:    "rgba(22,112,60,.10)"
      ok-border:  "rgba(22,112,60,.30)"
      wait-tint:  "rgba(122,82,0,.12)"
      wait-border: "rgba(122,82,0,.42)"
      fail-tint:  "rgba(168,47,34,.12)"
      fail-border: "rgba(168,47,34,.34)"

typography:
  families:
    ui:   "'IBM Plex Sans', system-ui, sans-serif"   # anything a human wrote
    mono: "'JetBrains Mono', monospace"               # anything a machine produced
  numeric: "tabular-nums"   # on for ALL digits, always
  # Ramp: weight size/line-height family · letter-spacing · use.
  ramp:
    display: { font: "600 30px/1.1 {typography.families.ui}",   tracking: "-0.7px", use: "page title, live timer/countdown" }
    section: { font: "600 23px/1.2 {typography.families.ui}",   tracking: "-0.3px", use: "section heading (h2)" }
    card:    { font: "600 19px/1.3 {typography.families.ui}",   tracking: "0",      use: "card title (h3)" }
    bar:     { font: "500 15px/1.4 {typography.families.ui}",   tracking: "0",      use: "app-bar brand, section label" }
    button:  { font: "500 13.5px/1 {typography.families.ui}",   tracking: "0",      use: "buttons, nav links, form labels" }
    body:    { font: "400 13px/1.6 {typography.families.ui}",   tracking: "0",      use: "body + all data (mono for data)" }
    log:     { font: "400 12.5px/1.65 {typography.families.mono}", tracking: "0",   use: "log tail, code, inputs, .mono cells" }
    keyword: { font: "600 11px/1 {typography.families.mono}",   tracking: "0.4px",  transform: "uppercase", use: "state keyword chips, th" }
    kicker:  { font: "600 11px/1 {typography.families.mono}",   tracking: "2.4px",  transform: "uppercase", color: "{colors.dark.accent.text}", use: "card eyebrow" }

rounded:
  chip:    "8px"    # chips, focus-ring radius, log/plan/input corners
  row:     "10px"   # declared; unused in web (no floating-row model shipped)
  control: "11px"   # buttons, text inputs
  card:    "14px"   # cards, banners, approve blocks
  shell:   "16px"   # declared; unused in web

spacing:
  row-h:     "44px"       # min hit target: row, control, button height
  header-h:  "60px"       # app bar
  tray-inset: "6px"       # declared; unused in web
  gap-inline: "8px"       # declared; unused in web
  gap-cell:  "16px"       # app-bar gaps, row grid columns
  gap-card:  "12px"       # between cards in the grid
  pad-card:  "22px 24px"  # card padding
  pad-page:  "26px 30px"  # main padding
  main-max:  "1100px"     # centered content column (app.css, not a token)
  phone-max: "390px"      # phone approval surface width (app.css)
  focus-offset: "2px"
  breakpoints: { phone: "719px" }   # @media (max-width:719px) — phone, and phone only; 720–1099 keeps desktop layouts

focus:
  ring:   "3px solid {colors.dark.accent.text}"   # :focus-visible only; never recolors to state
  offset: "2px"
  radius: "{rounded.chip}"

motion:
  breathe: "xc-breathe 1.6s infinite"   # opacity 1 -> .35; RUNNING dot/pin breathes, never spins
  reduced: "prefers-reduced-motion collapses all animation and transition"

components:
  appbar:
    height: "{spacing.header-h}"
    background: "{colors.dark.surface.rail}"
    border-bottom: "1px solid {colors.dark.surface.hairline}"
    sticky: true
    nav-link:        { height: "44px", radius: "{rounded.chip}", color: "{colors.dark.ink.400}", font: "{typography.ramp.button}" }
    nav-link-current: { background: "{colors.dark.accent.tint}", color: "{colors.dark.accent.text}" }
  card:
    background: "{colors.dark.surface.card}"
    border: "1px solid {colors.dark.surface.hairline-faint}"
    radius: "{rounded.card}"
    padding: "{spacing.pad-card}"
    grid: "repeat(2, minmax(0,1fr))"   # span2 spans both columns
  chip:                              # state pill: dot + uppercase keyword
    radius: "{rounded.chip}"
    padding: "3px 9px"
    font: "{typography.ramp.keyword}"
    variants:
      ok:   { color: "{colors.dark.signal.ok}",   border: "{colors.dark.signal.ok-border}",   background: "{colors.dark.signal.ok-tint}" }
      wait: { color: "{colors.dark.signal.wait}", border: "{colors.dark.signal.wait-border}", background: "{colors.dark.signal.wait-tint}" }
      fail: { color: "{colors.dark.signal.fail}", border: "{colors.dark.signal.fail-border}", background: "{colors.dark.signal.fail-tint}" }
      run:  { color: "{colors.dark.accent.text}", border: "{colors.dark.accent.border}",      background: "{colors.dark.accent.tint}", dot-motion: "{motion.breathe}" }
      dim:  { color: "{colors.dark.signal.dim-text}", dot: "{colors.dark.signal.dim-dot}" }
  button:
    min-height: "{spacing.row-h}"
    radius: "{rounded.control}"
    base:    { background: "{colors.dark.surface.control}", color: "{colors.dark.ink.100}", border: "1px solid {colors.dark.surface.hairline}" }
    primary: { background: "{colors.dark.accent.fill}", color: "{colors.dark.accent.on}", border: "transparent" }
    danger:  { background: "{colors.dark.signal.fail-tint}", color: "{colors.dark.signal.fail}", border: "{colors.dark.signal.fail-border}" }
  table:
    th:  { font: "{typography.ramp.keyword}", color: "{colors.dark.ink.400}", border-bottom: "1px solid {colors.dark.surface.hairline}" }
    td:  { color: "{colors.dark.ink.200}", border-bottom: "1px solid {colors.dark.surface.hairline-faint}" }
    row-hover: "{colors.dark.surface.tray}"
    stackable: "on phone (≤719px) each row becomes a labelled card (data-label from column)"
  banner:               # pause / silence / insecure, on every screen until lifted
    radius: "{rounded.card}"
    wait: { border: "{colors.dark.signal.wait-border}", background: "{colors.dark.signal.wait-tint}" }
    fail: { border: "{colors.dark.signal.fail-border}", background: "{colors.dark.signal.fail-tint}" }
  steprail:             # workflow ledger shape: a row of 10px state dots
    dot: { size: "10px", border: "1px solid {colors.dark.surface.hairline}", states: "ok/wait/fail/run/dim mirror chip colors" }
  approve-block:        # phone approval surface (390px), min 48px touch targets
    radius: "{rounded.card}"
    timer:   { border: "{colors.dark.signal.wait-border}", background: "{colors.dark.signal.wait-tint}" }
    destroy: { border: "{colors.dark.signal.fail-border}", background: "{colors.dark.signal.fail-tint}" }
  log:                  # streaming tail
    background: "{colors.dark.surface.void}"
    border: "1px solid {colors.dark.surface.hairline}"
    radius: "{rounded.chip}"
    font: "{typography.ramp.log}"
---

# exocomp web UI — design system

## Overview

exocomp is a single-operator homelab control plane. This document specifies the
design tokens for its web surface: a server-rendered HTML UI (templ + vanilla JS over a
JSON API, live updates via SSE, no SPA). It is the reference a change reads to keep
that UI consistent, and it maps onto the TUI where the two share a vocabulary.

The direction is clean, minimal, functional. The UI reports; it never sells. Five
principles drive every token here:

1. **Status before detail.** The dashboard answers "is everything OK?" without
   scrolling. Cards carry only signals that lead to a decision.
2. **Failure is louder than success.** Healthy states are quiet — dim ink, small
   dots. Failures, staleness, and waiting-on-human states carry the weight. This is
   why the palette spends its saturation on signal colors and keeps surfaces and
   text near-monochrome.
3. **Everything is a job.** One list, one detail view, one event stream. Components
   are shared (`chip`, `steprail`, table) rather than per-feature.
4. **Truth over reassurance.** Exit codes, timestamps, and serials render plainly.
   `tabular-nums` is on for every digit so columns align.
5. **Quiet surface, dense information.** Minimal chrome, a clear type hierarchy,
   tables that respect density. Whitespace serves scanning.

The token layer is deliberately small. `internal/web/tokens.css` owns the palette
and type ramp; `internal/web/app.css` applies them to layout and the accessibility
affordances (focus ring, touch targets, tabular-nums). A palette change is a
one-file edit.

**Two themes, both first-class.** Dark is the native theme; light is a full warm
remap, not an inversion. The in-page toggle cycles system -> light -> dark and
persists the choice; "system" carries no `data-theme` and follows
`prefers-color-scheme`. A pre-paint script applies the stored theme so a
light-mode operator never sees a dark flash.

## Sources

Three documents own the surface; read them together:

- [Invariants](INVARIANTS.md) — the design philosophy and hard rules that survive
  any redesign.
- The repo-root `DESIGN.md` — the machine tokens as a single contract.
- This document — the token spec and the component shapes that implement both.

`internal/web/tokens.css` and `internal/web/app.css` are the shipped truth. Where
a document disagrees with them, the code wins and the correction lands in
`docs/log.md`.

## Colors

Every color is a token; nothing is hard-coded at a call site. Tokens divide into
four families, each with a distinct job.

**Surfaces** form a depth ramp built from fill, not line. Each step up
(`void` -> `rail` -> `card` -> `tray` -> `control`) is a lighter fill in dark and a
distinct warm step in light. Hairlines survive in only two roles: a table header
rule (`hairline`) and inner cell dividers (`hairline-faint`). Depth is never drawn
with borders where a fill will do.

**Ink** is a five-step neutral ramp with measured contrast against the tray
surface. `ink-100` for headings and values, `ink-200` for body, `ink-400` for
labels and secondary text, `ink-500` for timestamps and meta — the last step that
still meets AA for text. `ink-600` is for dots and rules only and must never carry
text.

**Accent** is amethyst, deliberately split. `accent-fill` (`#A435CE` dark) is for
solid fills and primary buttons only — as text on a graphite surface it drops to
3.65:1 and fails AA. `accent-text` (`#BE7BEB` dark) is the accent as link, icon,
and focus ring. In light, one value (`#7A2FA6`) carries both roles. `accent-on` is
white text on a solid fill, shared by both themes.

**Signals** carry state and nothing else: green `ok`, amber `wait`, red `fail`, and
a dim neutral for queued/skipped/disabled. Each has a text color, a ~12% tint fill,
and a ~30% border, so a state reads as text, as a dot, and as a chip fill.
Running/interactive state uses the accent, not a signal color. State is never
color-only — every signal pairs with a label or glyph.

All pairings are annotated with their measured contrast ratio in `tokens.css` and
meet WCAG AA or better.

## Typography

Two families split by origin: **IBM Plex Sans** for anything a human wrote (UI
chrome, headings, body), **JetBrains Mono** for anything a machine produced (log
tails, code, serials, and the uppercase keyword/kicker labels). Both load from
Google Fonts with `system-ui` / `monospace` fallbacks. Pure-mono is on-brand for
this audience; the sans is the calmer sibling for prose.

`font-variant-numeric: tabular-nums` is global — every digit aligns, always. This
is load-bearing for scanning serials, exit codes, ages, and countdowns.

The ramp is nine named steps, each a single `font` shorthand token plus letter-
spacing:

| Token | Value | Role |
|---|---|---|
| `display` | 600 30px/1.1, -0.7px | page title, live timer/countdown |
| `section` | 600 23px/1.2, -0.3px | section heading (`h2`) |
| `card` | 600 19px/1.3 | card title (`h3`) |
| `bar` | 500 15px/1.4 | app-bar brand, section label |
| `button` | 500 13.5px/1 | buttons, nav links, form labels |
| `body` | 400 13px/1.6 | body text and all data |
| `log` | 400 12.5px/1.65 mono | log tail, code, inputs, `.mono` cells |
| `keyword` | 600 11px/1 mono, +0.4px, UPPERCASE | state chips, table headers |
| `kicker` | 600 11px/1 mono, +2.4px, UPPERCASE, accent | card eyebrow |

Content links carry no resting underline — 500 weight is the affordance — and the
underline returns on hover and focus as an interaction signal. `:visited` matches
default: this is a control plane, not a document corpus.

## Layout

The frame is a sticky app bar over a centered content column. The bar is
`header-h` (60px) tall, holds the brand, a horizontally scrolling primary nav, the
operator identity, and the theme toggle. On a narrow screen the nav scrolls within
its space (with a trailing-edge fade signalling more items) rather than pushing the
bar wider than the viewport. On phone the bar is a single 56px shell — menu ·
mark · page title · right meta — and the wordmark drops; the powerline foot is
sticky at 36px and `main` carries 50px bottom padding.

`main` is capped at 1100px and centered, padded by `pad-page` (26px 30px); on phone
it is 14px 16px with 50px bottom clearance. The dashboard lays cards on a
two-column grid (`gap-card`, 12px); a `span2` card spans both columns. Below 719px —
the one phone breakpoint — the grid collapses to one column; 720–1099 keeps the
desktop layout with the rail collapsed and `.config` at two columns. A third width
breakpoint is a bug until a SPEC says otherwise; touch hints are
`(pointer: coarse)`, never a width test.

Spacing is anchored by `row-h` (44px) — the minimum hit target, shared by rows,
controls, and buttons. On the phone approval surface (capped at 390px) touch
targets rise to 48px, an accessibility hard requirement.

Wide content never scrolls the page sideways. `body` uses `overflow-x: clip`, and
each wide block (`.tablewrap`, the nav) scrolls inside its own box. On phone
(≤719px), `stackable` tables transform: each row becomes a bordered card and each
cell prints its column name from `data-label`, so a row never loses its header. The
`thead` is visually hidden but kept in the accessibility tree.

## Elevation

There are no shadows. Depth is expressed entirely through the surface ramp: a
raised element sits on a lighter fill than the element behind it (`card` over
`void`, `tray` over `card`). The single exception the token system reserves is an
inset state ring drawn with `box-shadow: inset 0 0 0 1px <border>` for focus and
selection, not for drop-shadow elevation. This keeps the surface flat and quiet,
consistent with the "no gradients-as-decoration" anti-goal.

## Shapes

A radius ramp scales with element size: `chip` (8px) for chips, inputs, and the
log/plan boxes; `control` (11px) for buttons and text inputs; `card` (14px) for
cards, banners, and approval blocks. Corners are consistent within a size class so
the UI reads as one system.

Motion is minimal and meaningful. The only animation is `breathe` — a 1.6s opacity
pulse (1 -> .35) on the RUNNING dot and pin. Nothing spins; there is no progress
theatre. `prefers-reduced-motion` collapses all animation and transition to none.

## Components

**App bar and nav.** Rail-surface bar, hairline bottom border, sticky. Nav links
are 44px pills in `ink-400`; the current page fills with `accent-tint` and switches
to `accent-text`.

**Card.** The dashboard's unit. Card surface, faint hairline border, 14px radius,
`pad-card` padding. A `kicker` eyebrow (mono, wide-tracked, accent) names the
principle the card serves; an `h3` titles it. Empty states are a single quiet
`meta` line naming what would change the state.

**Chip.** The core state primitive: an 8px dot plus an uppercase mono keyword, in a
tinted pill. Five variants — `ok`, `wait`, `fail`, `run` (accent, breathing dot),
`dim` — map one-to-one to the signal palette. Never color-only.

**Step rail.** A row of 10px dots rendering a workflow's ledger at a glance, using
the same five states as the chip.

**Table.** Full-width, uppercase mono headers over a hairline rule, cells divided
by faint hairlines, hover paints the tray surface. Numeric and mono columns opt in
via `.num` / `.mono`; an unbounded mono value wraps inside a 32ch cap rather than
forcing the table to scroll. Real `<th scope>` throughout.

**Button.** Three variants at `row-h` min height: base (control surface), `primary`
(solid accent fill, white text), `danger` (fail tint, fail text and border). The
global-kill button adds a hold-to-confirm fill.

**Banner.** A full-width alert (pause / silence / insecure) on every screen until
the state lifts, carrying the count of swallowed alerts. Wait and fail variants use
the matching signal tint and border.

**Approval surface.** The phone flow (390px, 48px targets): risk-ordered blocks
with the fail-closed countdown in `display` type, destroys in their own red block,
the verbatim plan diff, and secret-backed values rendered as `[redacted · secret]`.
The phone never carries a value the web UI would not show.

**Log tail.** Void-surface box, mono, capped height, scrollable, with error lines
in fail color and system lines dimmed.

**Login.** A single card: paste a token, see the certificate fingerprint. Errors
in fail color, insecure-HTTP warned explicitly.

## Do's and Don'ts

**Do**

- Use a token for every color, size, and radius. A raw hex or px at a call site is
  a bug — the one-file palette edit depends on it.
- Pair every state with a label or glyph. Color alone never carries meaning.
- Keep `accent-fill` for solid fills and `accent-text` for text, links, and the
  focus ring. They are not interchangeable — the fill fails AA as text.
- Let failures and staleness carry visual weight; keep healthy states quiet.
- Keep `tabular-nums` on for every digit.
- Preserve the visible focus ring (3px, 2px offset). Removing `outline` without a
  replacement ring is a review-blocking bug; the ring stays amethyst even on an
  amber card.
- Design both themes together. Any new token needs a light and a dark value.

**Don't**

- No shadows for elevation; use the surface ramp.
- No gradients as decoration, no hero sections, no illustrations, no empty-state
  mascots. If a screen would look at home on a SaaS landing page, it is wrong.
- No spinners or progress theatre. The only motion is the RUNNING breathe, and it
  yields to `prefers-reduced-motion`.
- No `ink-600` as text — it is for dots and rules only.
- No metric cards without a decision behind them; no vanity metrics.
- No marketing language. The UI reports state; it does not reassure.
</content>

## Realignment v0.3 (landed)

The v0.3 handoff realigned the shipped surfaces; this section records the
components and the deliberate deviations that landed with it. Canon for values
remains `internal/web/tokens.css` and `internal/web/app.css`.

**App bar (F15).** The shell owns the page title in a slim bar (`--type-bar`,
15px/600) with a mono `who · scope` meta slot and the page's one primary Add
verb at the right. The 30px display h1 is gone from the list pages; the
`--header-h` token stays 60px (the sidebar brand shares it) while the bar
reads 58px.

**Grouped nav (F7).** The rail is grouped by verb — OPERATE / AUTOMATE /
PROVISION / ACCESS — each cluster under a mono label. Actionable-state badges
show beside Checks (down-target count, fail) and Approvals (pending, wait)
and Sources (never-synced, wait): a mono count, never a pill. Netgroups is a
tab on /checks; `/netgroups` is a redirect.

**Tray-table (F11).** A list surface's table floats inside a padded tray —
card padding collapses to 8px, the header reads as a quiet mono label with no
rule, and the rows alternate none/`--surface-tray` with hover on
`--surface-row`. No per-cell hairlines. The stackable phone transform and real
`<th scope>` are untouched. Applied to the 13 stackable list surfaces; the
deliberately non-stackable short tables (system, workflows) are untouched.

**Check cards (F1/F2/F3).** One card per check over a target grid on the tray
(`minmax(300px, 1fr)`). Reachability is the only chip; location is a fixed
meta slot in the row's second line — a route glyph + `on <netgroup>` (link to
Netgroups) · age, so the location claim reads down the column — never a chip
(invariant 5). A never-seen target is a single dim UNOBSERVED chip with
`on <netgroup> · away — not probed from here`, never a location chip. A down
tile carries the inset fail ring; the per-target clock right-aligns in the
tile header.

**Launch drawer (F4).** The source detail leads with identity and facts; the
launcher is a right-anchored 460px `<dialog>` opened by intent, JS-off via
`?launch=1` (the `open` attribute). The submit button is the bare verb
(`Queue run`); the footer states the contract (`asks nothing else · same as
exocomp run <source> <thing> [--egress …]`), so a long collection path wraps
the prose line and never the primary. The privileged toggle sits in its own
fail-tinted block. The Options fold holds the run-option toggles and timeout.

**Quiet/danger row actions (F8).** Row actions are 30px: `.btn.quiet`
(control fill, label ink) for Disable/Enable, `.btn.danger` (transparent,
fail ring) for Remove. Danger is a ring, not a fill, until the confirm.
Full-size `.btn.danger` (fail tint) remains for the decision/confirm buttons
(the phone Reject, the confirm dialog).

**Row actions (v0.6).** A tray row's actions cell holds at most two verb
glyphs — the actions an operator reaches for daily — then a chevron that opens
a menu of the rarer verbs and views. Fewer than three actions → glyphs only.
Glyph buttons are 34×34 (44×44 on phones) transparent icons, 18px glyphs
(20px on phones), ink-500 → ink-400 on row hover → surface-control fill +
ink-100 on button hover; the chevron is 26×34 (32×44 on phones), ink-600 →
ink-400, open state fills surface-control with an accent-border ring. The menu
is surface-control, min-width 220, radius 11; items are 36px glyph + word with
an optional mono count; destructive items sit last in `--signal-fail` with an
"asks first" note. On phones the chevron opens a bottom sheet (surface-control,
radius 16 top, a `{name} · {kind}` identity line) listing every action. The
glyph set is fixed per verb (plan, apply, enable, disable, open, rotate,
remove, bindings) as inline stroke SVGs; every glyph and the chevron carry an
aria-label. The trailing "…" is retired — a destructive button is named
plainly and the confirm dialog is the safety.

**Powerline segments (F10).** Each segment is dot + word in its signal color
and links to where it is fixed — dead-man none is wait with a fix verb
(`— set one`), ntfy off is deliberate dim, disk is ok until it drops under
10%. State is never color alone.

### Deliberate deviations (reviewed)

- **The secrets columns were kept** (Name/Kind/Source/Created/Rotated/Used/
  Expiry/Bindings) rather than the handoff render 1e's
  SECRET/CREATED/LAST-USED/USED-BY: expiry and kind are safety-relevant for a
  sealed-value inventory. The 1e metadata sentence and the
  `unbound — safe to remove` copy landed.
- **Sync is a CLI verb, not a web route.** The handoff 1d header's "Sync now"
  button has no backend (no queued sync job kind exists; sources_write.go
  documents the deliberate omission). The source detail instead shows the
  command (`exocomp source sync <name>`) next to the never-synced fact. The
  never-synced wait state is **git-only**: a path source's tree is already on
  the box and read directly, has no sync verb (`source sync` refuses it), and so
  reads `n/a (path source)` for Last sync and enables the launch select as soon
  as discovery finds playbooks.
- **TUI keymap.** The TUI's post-v1 screens grew past the handoff round's
  seven-screen model, so two literals were pushed back: the `[0-6]` help-bar
  range is the truthful `[0-9] [s] [u]` (the screen set grew), and `r` was
  freed for reject by remapping the runners jump to `u`. The load-bearing
  reserved-letters rule and the verbatim per-screen bars are honored. The
  `b`/`l`/`x` run-now keys are reserved but unwired — their spec construction
  lives in the CLI (package main), and forking it into the TUI would split
  the queue-facing contract.
