# exocomp — TUI design brief

The TUI is surface number one; the web UI is its calmer sibling. It exists (Jobs,
Backups, Workspaces, Schedules; `[1]-[4]`/tab; monochrome + one accent; served
locally and over SSH via wish) — this brief designs it deliberately instead of
letting it accrete. The web brief's doctrine applies; below is only what differs
in a terminal.

## The medium's constraints

- **Cells, not pixels.** Reference frames: 80×24 (fully operable — phone SSH is
  real) and 120×36 (comfortable default). Nothing requires more than 80×24;
  width earns columns, never capabilities.
- **Color is a hint, never the message.** Truecolor → 16-color ANSI → no color;
  every state survives as text. Signal mapping: ok=green, wait=yellow, fail=red,
  accent=magenta, with an explicit 16-color fallback table.
- **Latency is design.** Stable layouts, in-place cell updates, the 2s tick as
  the heartbeat; nothing animates faster.
- **Keyboard only.** Every action a key, every key visible in the help bar; no
  chords SSH clients eat; coherent map as screens grow.

## M3 surfaces to design

1. **Home `[0]`** — glance parity with the web dashboard's information: Needs-you
   (approvals + TTL, stale backup), queue with oldest-queued age, backup
   freshness incl. drill row, drift, next-to-fire, footer signal line
   (dead-man · ntfy · disk · version).
2. **Approvals `[5]`** — the approval surface contract terminal-rendered:
   risk-ordered detail, destroys first, `[redacted · secret]` inline, `a`/`r`
   with fail-closed TTL, stale-plan refusal state, plan diff in a pager. Open
   question: typed confirmation word for destroy-carrying approvals (mirroring
   the phone's hold-to-confirm)?
3. **Events `[6]`** — outcome-enum table with reasons; `e` opens the evaluation
   trace reflowed to 80 columns.
4. **Banner system** — pause/silence/both on every screen with swallowed count;
   `p` pauses reactive from anywhere; mute modal with mandatory expiry.
5. **First-run wizard** — the four-step flow as full-screen states; the kit page
   in ASCII, blocking on the typed confirmation.
6. **Empty states** — first-day voice: healthy-and-expectant one-liners naming
   the command that changes the state.

## Deliverables

Monospace artboards: home at 80×24 and 120×36, approvals detail, events + trace,
banner strip, wizard 1–2, one empty state. A keybinding map (global + per-screen,
collision-free, help-bar rendering). The 16-color fallback table. A per-screen
state inventory (loading / empty / populated / degraded-width).

## Anti-goals

No box-drawing ornament beyond structural rules, no ASCII-art logos, no spinners
faster than the data, no mouse-only affordances, nothing that breaks in tmux.
