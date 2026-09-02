---
version: alpha
name: exocomp
description: Design system for the exocomp control-plane web surface — a single-operator instrument panel in two first-class themes (dark native, light warm remap). Token values transcribe internal/web/tokens.css verbatim; this file is the token authority for agents.

colors:
  # Two complete palettes. Every token in `dark` has a `light` counterpart with
  # the same role. Contrast is measured against the surface each token sits on:
  # dark ink/signals vs surface-tray #171C1E; light ink/signals vs panel #FFFDF9.
  # Values are copied from internal/web/tokens.css verbatim.
  dark:
    surface-void:    "#0A0C0D"      # page backplate
    surface-rail:    "#0E1112"      # left nav, app bar
    surface-card:    "#121618"      # card, table shell
    surface-tray:    "#171C1E"      # raised tray, row group
    surface-row:     "#191F21"      # selected / active row
    surface-control: "#22282A"      # button, input, chip
    hairline:        "rgba(255,255,255,.09)"
    hairline-faint:  "rgba(255,255,255,.06)"
    ink-100: "#ECEFF0"              # 14.6:1 · headings, values
    ink-200: "#D6DBDD"              # 11.7:1 · body
    ink-400: "#A6AFB2"              #  7.5:1 · secondary, labels
    ink-500: "#8A9296"              #  5.4:1 · timestamps, meta, dim (last AA text step)
    ink-600: "#6B7477"              #  3.6:1 · dots, rules, dividers — NEVER text
    accent-fill:   "#A435CE"        # solid fills/buttons only · white text on it = 5.1:1
    accent-text:   "#BE7BEB"        # accent as text, icon, link, focus ring
    accent-on:     "#fff"           # text/icon on a solid accent fill (shared, both themes)
    accent-tint:   "rgba(164,53,206,.14)"
    accent-border: "rgba(190,123,235,.34)"
    signal-ok:   "#4FB86B"          # 7.9:1 · succeeded, fresh, in sync
    signal-wait: "#E3A008"          # 7.6:1 · awaiting approval, stale, drift
    signal-fail: "#F0645A"          # 5.4:1 · failed, timed out, interrupted
    signal-dim-text: "#8A9296"      # queued, skipped, disabled (text)
    signal-dim-dot:  "#6B7477"      # queued, skipped, disabled (dots only)
    ok-tint:   "rgba(79,184,107,.12)"
    ok-border: "rgba(79,184,107,.28)"
    wait-tint: "rgba(227,160,8,.12)"
    wait-border: "rgba(227,160,8,.30)"
    fail-tint: "rgba(240,100,90,.12)"
    fail-border: "rgba(240,100,90,.30)"
    primary: "#A435CE"              # alias for the accent fill
  light:                            # warm, low-chroma remap — lab notebook, not inverted console
    surface-void:    "#F5F2EC"      # page
    surface-rail:    "#EFEBE3"      # left nav, app bar
    surface-card:    "#FFFDF9"      # panel
    surface-tray:    "#FFFDF9"
    surface-row:     "#F1EDE5"      # selected / active row
    surface-control: "#EFEBE3"
    hairline:        "rgba(60,50,38,.16)"
    hairline-faint:  "rgba(60,50,38,.12)"
    ink-100: "#1D1A16"
    ink-200: "#3A352E"
    ink-400: "#6B655B"
    ink-500: "#6B655B"              # same as 400 in light
    ink-600: "rgba(60,50,38,.28)"   # rules/dots only
    accent-fill: "#7A2FA6"          # 7.4:1 · one accent carries text, buttons and rings
    accent-text: "#7A2FA6"          # same value as fill in light
    accent-on: "#fff"               # shared with dark, inherited
    accent-tint:   "rgba(122,47,166,.10)"
    accent-border: "rgba(122,47,166,.34)"
    signal-ok:   "#16703C"          # 6.2:1
    signal-wait: "#7A5200"          # 6.5:1
    signal-fail: "#A82F22"          # 5.7:1
    signal-dim-text: "#6B655B"
    signal-dim-dot:  "rgba(60,50,38,.40)"
    ok-tint:   "rgba(22,112,60,.10)"
    ok-border: "rgba(22,112,60,.30)"
    wait-tint: "rgba(122,82,0,.12)"
    wait-border: "rgba(122,82,0,.42)"
    fail-tint: "rgba(168,47,34,.12)"
    fail-border: "rgba(168,47,34,.34)"
    primary: "#7A2FA6"              # alias for the accent fill

typography:
  # Two families split by origin: IBM Plex Sans for anything a human wrote,
  # JetBrains Mono for anything a machine produced. Ligatures and contextual
  # alternates are OFF on the mono face (the W4 rule); tabular-nums is on for
  # every digit, always (expressed in prose — it is a global, not a level).
  family-ui:
    fontFamily: "'IBM Plex Sans', system-ui, sans-serif"
  family-mono:
    fontFamily: "'JetBrains Mono', monospace"
    fontFeature: '"liga" 0, "calt" 0'
  display:
    fontFamily: "'IBM Plex Sans', system-ui, sans-serif"
    fontSize: 30px
    fontWeight: 600
    lineHeight: 1.1
    letterSpacing: "-0.7px"   # page title, live timer
  section:
    fontFamily: "'IBM Plex Sans', system-ui, sans-serif"
    fontSize: 23px
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.3px"   # section heading (h2)
  card:
    fontFamily: "'IBM Plex Sans', system-ui, sans-serif"
    fontSize: 19px
    fontWeight: 600
    lineHeight: 1.3           # card title (h3)
  bar:
    fontFamily: "'IBM Plex Sans', system-ui, sans-serif"
    fontSize: 15px
    fontWeight: 500
    lineHeight: 1.4           # bar title, section label
  button:
    fontFamily: "'IBM Plex Sans', system-ui, sans-serif"
    fontSize: 13.5px
    fontWeight: 500
    lineHeight: 1             # buttons, nav links, form labels
  body:
    fontFamily: "'IBM Plex Sans', system-ui, sans-serif"
    fontSize: 13px
    fontWeight: 400
    lineHeight: 1.6           # body + all data (mono for data)
  log:
    fontFamily: "'JetBrains Mono', monospace"
    fontFeature: '"liga" 0, "calt" 0'
    fontSize: 12.5px
    fontWeight: 400
    lineHeight: 1.65          # log tail, code, inputs, .mono cells
  keyword:
    fontFamily: "'JetBrains Mono', monospace"
    fontFeature: '"liga" 0, "calt" 0'
    fontSize: 11px
    fontWeight: 600
    lineHeight: 1
    letterSpacing: 0.4px      # UPPERCASE state keywords
  kicker:
    fontFamily: "'JetBrains Mono', monospace"
    fontFeature: '"liga" 0, "calt" 0'
    fontSize: 11px
    fontWeight: 600
    lineHeight: 1
    letterSpacing: 2.4px      # UPPERCASE eyebrow, accent-text

rounded:
  chip: 8px        # chips, focus-ring radius, log/plan/input corners
  row: 10px        # tray-table rows, target tiles
  control: 11px    # buttons, text inputs
  card: 14px       # cards, banners, approve blocks, modals
  shell: 16px      # declared; reserved for the largest containers

spacing:
  row-h: 44px        # min hit target: job row, table row, control, button
  header-h: 60px     # app bar token (sidebar brand shares it; bar renders 58px)
  powerline-h: 34px  # fixed bottom status bar
  tray-inset: 6px    # rows float inside a padded tray
  gap-inline: 8px    # chip/badge clusters
  gap-cell: 16px     # row grid columns
  gap-card: 12px     # between cards in the grid
  pad-card: "22px 24px"
  pad-page: "26px 30px"
  nav-item-h: 40px   # sidebar nav pill height
  focus-offset: 2px
  breakpoint-stack-cards: 720px
  breakpoint-stack-tables: 560px
  breakpoint-drawer: 820px   # below this the sidebar is an off-canvas drawer
  phone-max: 390px           # phone approval surface width
  modal-width: "min(34rem, calc(100vw - 32px))"
  drawer-width: "min(460px, 100vw)"

components:
  # Components reference the dark palette; the light theme provides the same
  # roles under colors.light.*. Every component ships in both themes.
  chip:                       # state pill: 8px dot + uppercase mono keyword
    typography: "{typography.keyword}"
    rounded: "{rounded.chip}"
    padding: "3px 9px"
    height: 22px
    borderWidth: 1px
    borderColor: "{colors.dark.hairline}"
    dot-size: 8px
  chip-ok:
    color: "{colors.dark.signal-ok}"
    backgroundColor: "{colors.dark.ok-tint}"
    borderColor: "{colors.dark.ok-border}"
    dot-color: "{colors.dark.signal-ok}"
  chip-wait:
    color: "{colors.dark.signal-wait}"
    backgroundColor: "{colors.dark.wait-tint}"
    borderColor: "{colors.dark.wait-border}"
    dot-color: "{colors.dark.signal-wait}"
  chip-fail:
    color: "{colors.dark.signal-fail}"
    backgroundColor: "{colors.dark.fail-tint}"
    borderColor: "{colors.dark.fail-border}"
    dot-color: "{colors.dark.signal-fail}"
  chip-run:                   # running/interactive uses the accent, not a signal
    color: "{colors.dark.accent-text}"
    backgroundColor: "{colors.dark.accent-tint}"
    borderColor: "{colors.dark.accent-border}"
    dot-color: "{colors.dark.accent-text}"
    dot-motion: breathe       # the only animation in the system
  chip-dim:
    color: "{colors.dark.signal-dim-text}"
    dot-color: "{colors.dark.signal-dim-dot}"
  button:
    typography: "{typography.button}"
    rounded: "{rounded.control}"
    height: "{spacing.row-h}"
    padding: "0 18px"
    borderWidth: 1px
    backgroundColor: "{colors.dark.surface-control}"
    textColor: "{colors.dark.ink-100}"
    borderColor: "{colors.dark.hairline}"
  button-primary:
    backgroundColor: "{colors.dark.accent-fill}"
    textColor: "{colors.dark.accent-on}"
    borderColor: transparent
  button-danger:              # decision/confirm buttons (full-size)
    backgroundColor: "{colors.dark.fail-tint}"
    textColor: "{colors.dark.signal-fail}"
    borderColor: "{colors.dark.fail-border}"
  button-danger-row:          # row-scale danger: a ring, not a fill, until the confirm
    height: 30px
    backgroundColor: transparent
    textColor: "{colors.dark.signal-fail}"
    borderWidth: 0
    boxShadow: "inset 0 0 0 1px {colors.dark.fail-border}"
  button-quiet:               # row-scale Disable/Enable
    height: 30px
    backgroundColor: "{colors.dark.surface-control}"
    textColor: "{colors.dark.ink-400}"
    borderColor: "{colors.dark.hairline}"
  card:
    backgroundColor: "{colors.dark.surface-card}"
    borderColor: "{colors.dark.hairline-faint}"
    borderWidth: 1px
    rounded: "{rounded.card}"
    padding: "{spacing.pad-card}"
  tray-table:                 # list surface: rows float in a padded tray
    backgroundColor: "{colors.dark.surface-card}"
    rounded: "{rounded.card}"
    padding: 8px
    row-height: "{spacing.row-h}"
    row-radius: "{rounded.row}"
    row-hover: "{colors.dark.surface-tray}"
    row-active: "{colors.dark.surface-row}"
    header-color: "{colors.dark.ink-600}"
    header-typography: "600 10.5px/1 'JetBrains Mono', monospace, +0.8px uppercase"
    attention-tint: "{colors.dark.wait-tint}"
    attention-edge: "{colors.dark.wait-border}"
  check-card:                 # one card per check, over a target grid on the tray
    backgroundColor: "{colors.dark.surface-card}"
    rounded: "{rounded.card}"
    borderColor: "{colors.dark.hairline-faint}"
    borderWidth: 1px
    padding: "18px 22px 20px"
  target:                     # one tile = one probe
    backgroundColor: "{colors.dark.surface-tray}"
    rounded: "{rounded.row}"
    padding: "12px 14px"
  target-down:
    boxShadow: "inset 0 0 0 1px {colors.dark.fail-border}"
  launch-drawer:              # right-anchored dialog, opened by intent
    backgroundColor: "{colors.dark.surface-card}"
    width: "{spacing.drawer-width}"
    borderLeftColor: "{colors.dark.hairline}"
    borderLeftWidth: 1px
  nav-group:                  # rail organised by verb
    color: "{colors.dark.ink-600}"
    typography: "600 10px/1 'JetBrains Mono', monospace, +1.8px uppercase"
  nav-item:
    height: 40px
    rounded: "{rounded.chip}"
    typography: "{typography.button}"
    textColor: "{colors.dark.ink-200}"
    padding: "0 12px"
  nav-item-hover:
    backgroundColor: "{colors.dark.surface-tray}"
    textColor: "{colors.dark.ink-100}"
  nav-item-active:
    backgroundColor: "{colors.dark.accent-tint}"
    textColor: "{colors.dark.accent-text}"
  nav-badge:                  # actionable state count, a mono number never a pill
    typography: "600 10.5px/1 'JetBrains Mono', monospace"
  nav-badge-fail:
    color: "{colors.dark.signal-fail}"
  nav-badge-wait:
    color: "{colors.dark.signal-wait}"
  banner:                     # pause / silence / insecure, on every screen until lifted
    rounded: "{rounded.card}"
    backgroundColor: "{colors.dark.wait-tint}"
    borderColor: "{colors.dark.wait-border}"
    borderWidth: 1px
    textColor: "{colors.dark.ink-100}"
    padding: "14px 18px"
  banner-fail:
    backgroundColor: "{colors.dark.fail-tint}"
    borderColor: "{colors.dark.fail-border}"
  powerline:                  # fixed status bar at the viewport bottom
    height: "{spacing.powerline-h}"
    backgroundColor: "{colors.dark.surface-rail}"
    borderTopColor: "{colors.dark.hairline-faint}"
    textColor: "{colors.dark.ink-500}"
    typography: "{typography.log}"
  powerline-segment:          # dot + word in its signal color, links where fixed
    color: "{colors.dark.ink-500}"
    typography: "{typography.log}"
    dot-size: 6px
  powerline-segment-ok:
    color: "{colors.dark.signal-ok}"
  powerline-segment-wait:
    color: "{colors.dark.signal-wait}"
  powerline-segment-fail:
    color: "{colors.dark.signal-fail}"
  powerline-segment-dim:
    color: "{colors.dark.ink-500}"
    dot-color: "{colors.dark.ink-600}"
  step-dot:                   # workflow ledger: a row of state dots
    size: 10px
    rounded: 9999px
    borderWidth: 1px
    borderColor: "{colors.dark.hairline}"
    backgroundColor: "{colors.dark.signal-dim-dot}"
  step-dot-ok:
    backgroundColor: "{colors.dark.signal-ok}"
    borderColor: "{colors.dark.ok-border}"
  step-dot-wait:
    backgroundColor: "{colors.dark.signal-wait}"
    borderColor: "{colors.dark.wait-border}"
  step-dot-fail:
    backgroundColor: "{colors.dark.signal-fail}"
    borderColor: "{colors.dark.fail-border}"
  step-dot-run:
    backgroundColor: "{colors.dark.accent-text}"
    borderColor: "{colors.dark.accent-border}"
    motion: breathe
  modal:                      # native <dialog>, top layer, focus-trapped by the UA
    backgroundColor: "{colors.dark.surface-card}"
    textColor: "{colors.dark.ink-200}"
    borderColor: "{colors.dark.hairline-faint}"
    borderWidth: 1px
    rounded: "{rounded.card}"
    width: "{spacing.modal-width}"
    padding: "{spacing.pad-card}"
    backdropColor: "rgba(0,0,0,.5)"
  confirm-dialog:             # destructive actions go through the confirm
    backgroundColor: "{colors.dark.surface-card}"
    textColor: "{colors.dark.ink-200}"
    rounded: "{rounded.card}"
    padding: "{spacing.pad-card}"
    destroy-block:
      backgroundColor: "{colors.dark.fail-tint}"
      borderColor: "{colors.dark.fail-border}"
      rounded: "{rounded.control}"
---

# exocomp — design system

## Overview

exocomp is a single-operator homelab control plane. Its web surface is a
server-rendered instrument panel, not a SaaS dashboard: the UI makes state
legible and decisions fast — it never reassures and never markets. Healthy is
quiet, failure is loud. A nominal system renders quiet counts and an empty
attention band; signal colour is spent only where a decision is needed. State
is a word plus a colour, never colour alone: every signal pairs a dot and an
uppercase mono keyword that survives greyscale.

There are two themes and both are first-class. Dark is the native theme; light
is a warm low-chroma remap, a lab notebook rather than an inverted console. The
in-page toggle cycles system -> light -> dark; a palette change is a
`tokens.css`-only edit and every component ships in both themes.

Depth is the surface ramp, never shadows: each step up is a lighter fill, and
hairlines survive in exactly two places — a wide table's header rule and an
inset state ring. There are no shadows, gradients, hero sections, illustrations,
mascots, or spinners. The only motion is the RUNNING breathe, and it yields to
`prefers-reduced-motion`.

This file is the token authority. Every value transcribes
`internal/web/tokens.css` and `internal/web/app.css` verbatim; if a value here
disagrees with the CSS, the CSS wins and this file is corrected.

## Colors

Every colour is a token; nothing is hard-coded at a call site. Tokens divide
into four families, each with a distinct job.

**Surfaces** form a depth ramp built from fill, not line: `surface-void` ->
`surface-rail` -> `surface-card` -> `surface-tray` -> `surface-control`, each a
lighter fill in dark and a distinct warm step in light. Hairlines survive in
two roles only — `hairline` for a table header rule, `hairline-faint` for inner
cell dividers. Depth is never drawn with borders where a fill will do.

**Ink** is a five-step neutral ramp with measured contrast against the tray
surface: `ink-100` for headings and values, `ink-200` for body, `ink-400` for
labels and secondary text, `ink-500` for timestamps and meta — the last step
that still meets AA for text. `ink-600` (3.6:1 dark) is for dots and rules only
and must never carry text.

**Accent** is amethyst, deliberately split. `accent-fill` (`#A435CE` dark) is
for solid fills and primary buttons only — as text on graphite it drops to
3.65:1 and fails AA. `accent-text` (`#BE7BEB` dark) is the accent as link, icon,
and focus ring. In light, one value (`#7A2FA6`) carries both roles. `accent-on`
is white text on a solid fill, shared by both themes. `primary` is an alias for
the accent fill, so consumers that read a conventional primary palette resolve
`{colors.dark.primary}` / `{colors.light.primary}` to the same value.

**Signals** carry state and nothing else: `signal-ok`, `signal-wait`,
`signal-fail`, and the dim neutral for queued/skipped/disabled
(`signal-dim-text`, `signal-dim-dot`). Each state colour pairs a `-tint` fill
(~10–14% alpha) and a `-border` (~30% alpha) for its chip and ring. Running
and interactive state uses the accent, never a signal colour.

All pairings are annotated with their measured contrast ratio and meet WCAG AA
or better. Contrast is measured against the surface each token actually sits on
(dark vs `surface-tray` `#171C1E`, light vs panel `#FFFDF9`).

## Typography

Two families split by origin. **IBM Plex Sans** is for anything a human wrote —
UI chrome, headings, body. **JetBrains Mono** is for anything a machine
produced — log tails, code, serials, timestamps, and the uppercase
keyword/kicker labels. `system-ui` and `monospace` are the fallbacks.

`font-variant-numeric: tabular-nums` is global: every digit aligns, always.
This is load-bearing for scanning serials, exit codes, ages, and countdowns.

Ligatures and contextual alternates are OFF on the mono face
(`font-feature-settings: "liga" 0, "calt" 0`). A mono value must never render a
glyph the machine did not emit; the face stays literal.

The ramp is nine named levels, each a `typography.*` token:

| Token | Value | Role |
|---|---|---|
| `display` | 600 30px/1.1, -0.7px | page title, live timer/countdown |
| `section` | 600 23px/1.2, -0.3px | section heading (`h2`) |
| `card` | 600 19px/1.3 | card title (`h3`) |
| `bar` | 500 15px/1.4 | app-bar title, section label |
| `button` | 500 13.5px/1 | buttons, nav links, form labels |
| `body` | 400 13px/1.6 | body text and all data |
| `log` | 400 12.5px/1.65 mono | log tail, code, inputs, `.mono` cells |
| `keyword` | 600 11px/1 mono, +0.4px, UPPERCASE | state chips, table headers |
| `kicker` | 600 11px/1 mono, +2.4px, UPPERCASE, accent | card eyebrow |

Content links carry no resting underline — 500 weight is the affordance — and
the underline returns on hover and focus. `:visited` matches default: this is a
control plane, not a document corpus.

## Layout

The frame is a fixed-width **sidebar rail** (232px) plus a **content column**,
with a fixed **powerline** (34px) spanning the viewport bottom. The sidebar
ends above the powerline; `main` pads its content clear of it. On desktop the
rail collapses to a 64px icon-only rail; below 820px it becomes an off-canvas
drawer behind a slim top bar.

The **app bar** renders 58px (the `header-h` token stays 60px, shared with the
sidebar brand): the page title in `bar` type, a mono `who · scope` meta slot,
and the page's one primary verb at the right.

`main` is padded by `pad-page` (26px 30px). Cards pack into equal columns
(`auto-fill, minmax(360px, 1fr)`, gap 12px) and stack to one column below
720px. Below 560px, `stackable` tables transform: each row becomes a bordered
card and each cell prints its column name from `data-label`, so a row never
loses its header; the `thead` is visually hidden but stays in the a11y tree.

The rail is grouped by verb — OPERATE / AUTOMATE / PROVISION / ACCESS — each
cluster under a mono group label. Actionable-state badges are mono counts in
the fail or wait signal (down targets, pending approvals, never-synced
sources), never pills.

Spacing is anchored by `row-h` (44px), the desktop minimum hit target shared by
rows, controls, and buttons. On the phone approval surface (390px) touch
targets rise to 48px, an accessibility hard requirement. Wide content never
scrolls the page sideways; each wide block scrolls in its own box.

## Elevation & Depth

There are no shadows. Depth is expressed entirely through the surface ramp: a
raised element sits on a lighter fill than the element behind it (`card` over
`void`, `tray` over `card`). The one box-shadow the system allows is an inset
state ring — `inset 0 0 0 1px <state-border>` — used for a down target tile, a
row-scale danger button, and focus/selection. It is a state marker, never an
elevation shadow.

Motion is minimal and meaningful. The only animation is the RUNNING
**breathe**: a 1.6s opacity pulse (1 -> .35) on the RUNNING dot and pin.
Nothing spins; there is no progress theatre. `prefers-reduced-motion` collapses
all animation and transition to none.

## Shapes

A radius ramp scaled to element size: `chip` (8px) for chips, focus rings, and
log/plan boxes; `row` (10px) for tray-table rows and target tiles; `control`
(11px) for buttons and text inputs; `card` (14px) for cards, banners, and
approval blocks; `shell` (16px) reserved for the largest containers. Corners
are consistent within a size class so the UI reads as one system. Dots are
perfect circles at 6px (powerline), 8px (chip), and 10px (step rail).

## Components

**Chip.** The core state primitive: an 8px dot plus an uppercase mono keyword
in a tinted pill. Five variants — `ok`, `wait`, `fail`, `run` (accent,
breathing dot), `dim` — map one-to-one to the signal palette. Never colour
alone.

**Button.** Base (control surface, body ink), `primary` (solid accent fill,
`accent-on` text — the one place the fill is allowed), `danger` (fail tint) for
decision buttons, and the row-scale `quiet`/`danger` pair at 30px. Row-scale
danger is a ring, not a fill, until the confirm dialog; its label carries a
trailing "…".

**Card.** The dashboard's unit: card surface, faint hairline border, 14px
radius, `pad-card` padding. A `kicker` eyebrow names the principle the card
serves; an `h3` titles it.

**Tray-table.** A list surface's table floats inside a padded tray — card
padding collapses to 8px, the header reads as a quiet mono label with no rule,
and 44px rows alternate none/`surface-tray` with hover on `surface-row`. No
per-cell hairlines. An attention row (never-synced source) tints wait and
carries a 3px wait edge. Real `<th scope>` throughout.

**Check card / target grid.** One card per check over a tray-backed grid
(`minmax(300px, 1fr)`). Each tile is one probe: name, address, right-aligned
clock, the reachability chip, and location as prose. A down tile carries the
inset fail ring; an unobserved tile shows the dim chip with its cold-start
reason.

**Launch drawer.** The mutation surface, a right-anchored 460px `<dialog>`
opened by intent. The submit button states the contract ("Queue run — …"); the
privileged escalation sits in its own fail-tinted block.

**Nav.** Grouped rail by verb with mono labels; 40px pills, the active one on
`accent-tint` with `accent-text`. Badges are mono state counts in fail/wait.

**Banner.** A full-width alert (pause / silence / insecure) on every screen
until the state lifts, wait-tinted by default, fail variant for failures,
carrying the count of swallowed alerts.

**Powerline.** Fixed status bar; each segment is dot + word in its signal color
and links to where it is fixed. A gap reads as a verb — dead-man none is wait,
ntfy off is deliberate dim, disk is ok until it threatens.

**Step rail.** A row of 10px dots rendering a workflow ledger at a glance, using
the same five states as the chip.

**Modal / confirm dialog.** Native `<dialog>` in the top layer, focus-trapped
by the UA. Confirm dialogs carry the destructive contract in a fail-tinted
block; the destructive verb is a full-size danger button.

## Do's and Don'ts

**Do**

- Use a token for every colour, size, and radius. A raw hex or px at a call
  site is a bug — the one-file palette edit depends on it.
- Pair every state with a word plus a colour; colour alone never carries
  meaning, and every state must survive greyscale.
- Keep `accent-fill` for solid fills and `accent-text` for text, links, and the
  focus ring. They are not interchangeable — the fill fails AA as text.
- Keep `tabular-nums` on for every digit, and ligatures off on the mono face.
- Let empty states name the verb that changes them ("dead-man none — set one",
  "exocomp schedule add"). A gap is a signal, not a grey fact.
- Render a never-synced or not-yet-seen value as wait: an absence reads as a
  gap to fix, not as failure and not as a neutral label.
- Design both themes together. A new token needs a dark and a light value; a
  component that ships in one theme only is a bug.
- Meet the a11y gates: 48px touch targets on the phone surface (44px rows on
  desktop), 200% reflow, every control keyboard-reachable, greyscale legible,
  and a visible focus ring.
- Preserve the focus ring — 3px, 2px offset, `:focus-visible` only, amethyst
  even on an amber card. Removing `outline` without a replacement ring is a
  review-blocking bug.
- Let failures and staleness carry visual weight; keep healthy states quiet.

**Don't**

- Don't use `ink-600` as text — dots, rules, and dividers only.
- No shadows for elevation; use the surface ramp. No gradients, hero sections,
  illustrations, empty-state mascots, spinners, or progress theatre. The only
  motion is the RUNNING breathe, reduced-motion-respecting.
- No metric card without a decision behind it; no vanity metrics.
- No marketing language. The UI reports state; it does not reassure.
- Never ship a colour or state without its pair: a state chip with no label, a
  highlight with no resolving verb, is decoration.