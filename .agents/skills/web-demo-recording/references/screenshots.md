# Screenshots: capture workflow

Screenshots are the demo's **ground truth** — they capture the live DOM at each
step, and they are the anchors for the step cut (`trimming.md`).

## Capture

```sh
mkdir -p .agents/brain/screenshots/<proj>/
playwright-cli screenshot --filename=.agents/brain/screenshots/<proj>/<NN>-<step>.png
```

- **Naming**: zero-padded sequence + a short step name (`01-dark-dashboard`,
  `02-workspaces-list`, …). The sequence IS the demo order and the cut anchors.
- **Per step**: one screenshot at the moment the step's key fact is visible
  (a populated table, a dialog open, a diff expanded).
- The mtime of each PNG is the wall-clock capture time — used to map steps to
  video time (`trimming.md`).

## What makes a useful screenshot

- The step's **one fact** is legible (the thing you'd narrate).
- If the step has a scrollable area (a log, a diff), **scroll it to the end**
  first so the tail is visible: `playwright-cli --raw eval "() => { const l =
  document.querySelector('.log'); l.scrollTo(0,l.scrollHeight); return l.scrollTop; }"`.
- Confirm dialogs: screenshot with the dialog open (it's the deliverable for an
  apply/destroy step).

## Storage

Keep them in `.agents/brain/screenshots/<proj>/` (local-only). The demo video
lives beside them.

## Why screenshots beat video for verification

The video recorder can drift (a mid-recording light excursion, a reload flash)
while the live page stays correct. When they disagree, the screenshots win —
re-cut the affected video window, don't question the page.
