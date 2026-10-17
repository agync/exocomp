---
type: Guide
title: "Landing site"
description: "Building the exocomp.dev landing page together with the rendered docs bundle, with podman or with a CI toolchain, and the Cloudflare build that publishes them."
status: stable
tags: [operations, site, deployment]
---

# Landing site

`site/` holds the landing page and renders this bundle into a wiki that is served beside it
under `/docs/`. One build produces both halves, and the result is what gets published.

## What the build produces

| Output | Source |
|---|---|
| `dist/index.html`, `dist/styles/`, `dist/js/`, `dist/assets/` | `site/index.html` and the assets it references |
| `dist/docs/` | `docs/`, rendered by okf-wiki with `--base /docs/` |
| `dist/_headers`, `dist/_redirects`, `dist/robots.txt`, `dist/sitemap.xml` | copied verbatim |

`dist/` is build output: it is untracked, regenerated whole on every build, and never
hand-edited. The render copies the pinned mermaid runtime into `dist/docs/vendor/`, because
this bundle has diagrams and a render that would fall back to a public CDN is an error
rather than a quiet downgrade.

## Two renderers, one output

`site/build.sh` renders with whichever of these is available:

| Renderer | Used when | Notes |
|---|---|---|
| `podman` run of `ghcr.io/abn/okf-wiki:latest` | podman is installed | The reference render. Override the tag with `OKF_WIKI_IMAGE`. |
| A standalone `okf-wiki` binary | no podman, which is what a CI build image is | Found via `OKF_WIKI_BIN`, then `site/.toolchain/bin/okf-wiki`, then `okf-wiki` on `PATH`. |

`site/toolchain.sh` produces the second renderer for a host that has neither podman nor a
published binary: `go install` for `github.com/abn/okf-wiki/cmd/okf-wiki` into
`site/.toolchain/bin/`, then the same pinned mermaid bundle the image bakes, built with npm
and esbuild from that module's source. It needs Go 1.25 or newer and Node with npm, and it
writes only to `site/.toolchain/`, which is untracked. `build.sh` runs it on its own when
neither renderer is present, so a CI build command is just the build script.

```bash
make site/build     # renders and assembles site/dist/
make site/serve     # builds, then serves site/dist on :8788 (SITE_PORT=)
site/toolchain.sh   # only to install the renderer ahead of a build
```

`site/serve` runs `wrangler dev`, which reads the same config the deployment does. A plain
file server would ignore `_headers` and `_redirects`, and a preview that ignores them is not
a preview of the deployment; on a host with no Node, `python3 -m http.server -d site/dist`
is enough to read the pages.

| Variable | Effect |
|---|---|
| `OKF_WIKI_IMAGE` | Renderer image tag for the podman path |
| `OKF_WIKI_BIN` | Path to a renderer binary; wins over everything else |
| `OKF_WIKI_VENDOR` | Directory holding `mermaid-bundle.min.mjs`; defaults to the toolchain's copy |
| `OKF_WIKI_REF` | Source tag or commit `toolchain.sh` builds from |

## Publishing to Cloudflare

The site is the Worker `exocomp`, serving `exocomp.dev` as static assets with no script.
`site/wrangler.toml` names the build output — `[assets] directory = "./dist"` — and
`wrangler deploy` uploads that tree, `_headers` and `_redirects` included. The Worker's name
in the dashboard must match the config's `name`.

```bash
cd site && npx wrangler deploy   # local deploy; needs `wrangler login`
```

The Git-connected build lives under the Worker's **Settings → Build**:

| Setting | Value |
|---|---|
| Production branch | `main` |
| Root directory | `site` |
| Build command | `./build.sh` |
| Deploy command | `npx wrangler deploy` |
| Preview command | `npx wrangler preview` |
| Build variable `GO_VERSION` | the 1.25 line |

The root directory has to be `site` and not the repository root: it is both where the build
command runs and where the deploy command reads this config from. Set `GO_VERSION` as well —
the build image defaults to Go 1.24.3 and the renderer's module asks for 1.25, so without it
every build spends a minute downloading a newer toolchain first. `exocomp.dev` is added under
the Worker's **Domains & Routes**.

The Preview command is not a smaller deploy. A pull request build runs Cloudflare's Previews
mechanism, which serves that branch at a Preview URL and does not inherit production
settings, so nothing it does reaches `exocomp.dev`. It reads its own configuration from a
`previews` table and refuses to run without one. Every setting this Worker uses is top level,
`assets` and `compatibility_date` included, which is the case Cloudflare's guidance answers
with an empty block, and that is what `site/wrangler.toml` declares. The table has to sit
above `[assets]` in the file, or its keys parse into `[assets]` and the block reads as
missing again.

A Workers Builds run is two commands in one environment: the build command in the root
directory, then the deploy command, each on a fresh Ubuntu 24.04 sandbox per push. That
sandbox has Go and npm but no container runtime, which is why the build renders with a
standalone binary and installs its toolchain itself. The output path is not a dashboard
field: the deploy command takes it from `[assets] directory`, so the build command only has
to finish writing `dist/` before the deploy command starts.
