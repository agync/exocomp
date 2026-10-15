---
type: Guide
title: "Landing site"
description: "Building the exocomp.dev landing page together with the rendered docs bundle, with podman or with a CI toolchain, and the Cloudflare Pages settings that publish them."
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
site/build.sh       # renders and assembles site/dist/
site/toolchain.sh   # only to install the renderer ahead of a build
```

| Variable | Effect |
|---|---|
| `OKF_WIKI_IMAGE` | Renderer image tag for the podman path |
| `OKF_WIKI_BIN` | Path to a renderer binary; wins over everything else |
| `OKF_WIKI_VENDOR` | Directory holding `mermaid-bundle.min.mjs`; defaults to the toolchain's copy |
| `OKF_WIKI_REF` | Source tag or commit `toolchain.sh` builds from |

## Publishing to Cloudflare Pages

The Pages project is `exocomp` and serves `exocomp.dev`. The production branch is `main`;
every other branch is a preview deployment of the same build.

Two ways to deploy, and the config file speaks to the first:

```bash
cd site && npx wrangler pages deploy dist --project-name exocomp
```

For a Git-integrated project, create it under **Workers & Pages → Create application →
Pages → Connect to Git**. Build command and output directory are both relative to the root
directory, so the two choices travel together:

| Root directory (advanced) | Build command | Build output directory |
|---|---|---|
| repository root (the default) | `bash site/build.sh` | `site/dist` |
| `site` | `./build.sh` | `dist` |

Set the build variable `GO_VERSION` to the 1.25 line: the Pages build image defaults to Go
1.24.3 and the renderer's module asks for 1.25, so without it every build spends a minute
downloading a newer toolchain first.

The **Workers Builds** setup form — the one whose deploy command is `npx wrangler deploy` —
is a different product. This repository's `site/wrangler.toml` carries
`pages_build_output_dir`, which marks the project as Pages, and `wrangler deploy` reads that
key as Pages configuration; a Worker deploy would take an `[assets]` block instead. Until
that trade is wanted, the form to fill in is the Pages one.
