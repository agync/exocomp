# Branded wiki figures

The flow and architecture diagrams in `docs/` are images, not Mermaid or ASCII
art. This is the rig that produces them, so a figure in the wiki is
reproducible rather than hand-made. The per-figure sources live in `src/`,
beside the shell and fonts; the rendered WebP files land in
`docs/design/assets/figures/`.

```
./render.sh                       # every figure
./render.sh architecture-overview # one figure
```

Requires Google Chrome (or Chromium) and ImageMagick; the render needs no
network.

Output lands in `docs/design/assets/figures/` as WebP at quality 80, 1600 device
px. The wiki shows them at roughly half that, so every value in the HTML and
`shell.css` is 2x.

## What is here

| Path | Role |
|---|---|
| `shell.css` | the exocomp card shell: bar, chip, canvas, caption, tiles, sequence and terminal primitives |
| `fonts/` | the three woff2 faces the shell uses, vendored so a render needs no network; both families are SIL OFL 1.1 (`fonts/OFL-*.txt`) |
| `src/<name>.html` | one figure; `<name>.webp` is the output |
| `render.sh` | publishes each page's height, then screenshots at it |

The palette and type follow `internal/web/tokens.css`: amethyst accent, graphite
surface ramp, IBM Plex Sans for labels and JetBrains Mono for anything a
machine produced. `shell.css` transcribes those values, so a token change needs
the same edit here. There are no shadows, gradients, mascots or faux controls.

## Rules for a figure

- Wide, not tall. The aspect guard in `render.sh` refuses anything outside
  roughly 2.8:1 to 1.2:1; a tall figure pushes the prose down the page.
- Dark in both themes. The docs theme may be light; a dark card reads as one of
  the app's own panels, so do not try to make it theme-reactive.
- State is a word plus a colour, never colour alone. The sequence primitives
  carry their state as text (`optional`, `fired`, `error`).
- Never invent a number. Draw the structure and label it as structure.
- No `EXPAND` or `COPY` chrome: a drawn control gets clicked.

## Changing a figure

Edit its HTML, run `./render.sh <name>`, then inspect a 1:1 crop of every join
and connector. A whole-image glance misses what ships. Commit the HTML and the
WebP together; the page that embeds it should describe the same mechanism.

The figure is a derivative of `docs/`. When a mechanism changes, the page and
its figure change in the same commit series.
