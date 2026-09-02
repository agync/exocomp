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
8. **The queue is the only executor.** Every run button says what it does: "Queue
   run — deploy/site.yml @ main". Nothing in the UI executes directly.
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
  possible; trailing "…" only when the action opens a confirm dialog.
  Findings-not-praise tone; no marketing language.
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

## Component vocabulary (one of each, everywhere)

chip · card · tray-table (44px radius-10 rows in a padded card) · quiet/danger
buttons (danger is a ring until confirmed) · banner · native dialog + confirm ·
step rail · log tail · powerline (signal segments) · grouped nav rail
(OPERATE / AUTOMATE / PROVISION / ACCESS + System foot).

Component token specs and shipped CSS: `internal/web/tokens.css`,
`internal/web/app.css`, [DESIGN.md](DESIGN.md).