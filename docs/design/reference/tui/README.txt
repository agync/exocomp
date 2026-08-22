exocomp handoff · tui/ · v0.2 · 2026-08-22
text is the implementation. frames/ are golden-file fixtures for View() tests.

conventions
- full frames (…-80x24 / …-120x36) have exactly 24/36 rows; rows with
  right-aligned content are padded to exactly 80/120 columns; left-only rows
  carry no trailing spaces. rules are exactly width ─ characters.
- fragments (modals, banners, refusal, events, trace) are exact-width rows
  without a fixed row count; modal boxes are centered in their host width.
- unicode used: · ─ ▌ ▸ ● ▍ → ↗ ✓ … ┌ ┐ └ ┘ │ — plus the em dash.
  ASCII fallbacks: ▸ → ">" · ▌ → "!!" (see colors.txt).
- ▍ is the rendered cursor character in text inputs.
- selection is styling (reverse video), not characters — the mute modal's
  "1h" chip and the events table's first row are selected in the mock;
  plain text cannot carry that. the ▸ prefix marks the selected table row.
- all sample data is frozen fixture data; derived strings (ages, countdowns,
  sizes, versions) render from the implementation, not from these files —
  pin the clock/data in tests to match.
- commands render as `exocomp`, never `exo`.

contents
  frames/    12 character grids (see states.txt for the screen ↔ frame map)
  keymap.txt global + per-screen keys, reserved letters, row-24 help bars
  colors.txt semantic → truecolor → ANSI-16 → no-color text form
  copy.txt   load-bearing strings verbatim; {braces} mark computed segments
  states.txt loading/empty/populated/degraded-width per screen
