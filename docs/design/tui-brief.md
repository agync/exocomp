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

## Surfaces to design

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

## Settled by the design exploration (2026-08-22) — binding, with reconciliations

The TUI canvas delivered every deliverable above. Adopted as implementation
contract for the TUI work:

- **Destroy-approval confirms by typing the workspace name** — the terminal's
  hold-to-confirm, mirroring the phone contract. Stale plans render with no
  approve key at all ("fail closed — no approve key offered").
- **The keybinding map is the spec**: no F-keys, no Alt-combos, nothing tmux
  eats; one meaning per letter app-wide; reserved letters as listed; help-bar
  renderings per screen. Two deltas from today's code to adopt: `q` quits only
  at root (esc walks back), and `[0]` is the Home screen.
- **The 16-color fallback table and no-color text forms** ship as part of the
  theme implementation, not documentation.
- **Wizard deltas**: setup collects an operator display name and generates the
  ntfy topic (test message at step 4); nothing is written to disk until step 4
  — stronger than today's immediate `init`, adopted as the `exocomp setup` flow.

Reconciliation the engine must answer:

- The banner splits **PAUSED** (reactive jobs held) from **MUTED**
  (notifications held, jobs still run). The engine has silences and the kill
  switch but no notification-mute; adopt it as a notify-layer mute (mandatory
  expiry, logged reason) so the banner's three variants are all real states.
