---
type: Reference
title: Web UI renders
description: "Screen renders of the shipped web surface: the v0.3 realignment set and the dashboard in both themes."
status: stable
tags: [design, web, renders]
---

# Web UI renders

Screen renders of the shipped web surface. They show what the design tokens in
`internal/web/tokens.css` produce in a browser, which is the one thing the prose
pages cannot carry: the products, the spacing, and the dark theme as the native
one.

Two sets. The six `1a` to `1f` renders are the v0.3 realignment, taken surface by
surface. The two dashboard renders are the same screen in dark and light.

## The v0.3 realignment set

| Render | Shows |
|---|---|
| [1a shell and dashboard](assets/web/1a-shell-dashboard.png) | The app shell with the grouped nav rail, and the dashboard in its attention-first form: two cards that need a decision, an idle queue, and four zeroed cards that say what to run to change them |
| [1b checks](assets/web/1b-checks.png) | A card per check with its targets in a grid, the Checks, Probes and Netgroups tabs, and the reachability states a target can carry |
| [1c sources](assets/web/1c-sources.png) | The sources list as rows that open a detail, with the sync and trust columns a run cites |
| [1d source detail](assets/web/1d-source-detail.png) | Reading first and launching second, with the launch form collapsed to a focused drawer on the right |
| [1e secrets](assets/web/1e-secrets.png) | Metadata only, with rotate quiet and remove guarded, and the binding that spends a secret |
| [1f shared vocabulary](assets/web/1f-vocabulary.png) | The state chips, the powerline strip and the nav map on one screen: colour plus word, never colour alone, and location as prose |

## The dashboard in both themes

| Render | Shows |
|---|---|
| [dashboard-dark.png](assets/web/dashboard-dark.png) | Dark, the native theme |
| [dashboard-light.png](assets/web/dashboard-light.png) | Light, the full remap |

## How current these are

The realignment renders were taken during the v0.3 pass, before the follow-on
round of fixes. They are accurate on layout, tokens, state vocabulary and the
shell, and stale in two places:

- The sources list in render `1c` still carries an ENABLED
  column. The shipped list does not render it at the default.
- The launch drawer in render `1d` submits with the old
  button label, `Queue run — deploy/site.yml @ main`. The shipped button is the
  bare verb, with the contract carried on the footer line.

The dashboard renders predate the same round and carry the same caveat. Treat the
running server as the source of truth for anything that is not layout, tokens or
vocabulary.

## The vocabulary they encode

Three rules recur across the set, and each is visible in a render rather than
described in one.

State is colour plus word. A down target is a red chip that also says DOWN, and
unobserved is a third state that is neither up nor down. Location is prose, not a
chip: an address reads as on home, because reachability and location are different
facts.

The powerline carries gaps as signals. An unset dead-man switch is a wait state
with a verb, not a grey label. The strip is the same in both themes, and the TUI's
row 24 mirrors it.

Destruction is separated from its neighbours. Rotating a secret is quiet,
removing one is guarded, and each sits on the source or secret page next to what
it affects rather than in a row action.
