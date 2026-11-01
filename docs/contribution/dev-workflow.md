---
type: Guide
title: "Developer workflow"
description: "Running exocompd in local development mode, generating templ components, and debugging."
status: stable
tags: [contributing, dev, workflow, templ, debugging]
---

# Developer workflow

This guide covers running and debugging `exocompd` on a local workstation during active development.

## Running the local daemon

Run `exocomp serve` against an isolated development data directory. The binary
resolves its data dir from `XDG_DATA_HOME`, so override that rather than passing a
flag:

```bash
mkdir -p /tmp/exocomp-dev
XDG_DATA_HOME=/tmp/exocomp-dev ./bin/exocomp init
XDG_DATA_HOME=/tmp/exocomp-dev ./bin/exocomp token new web
XDG_DATA_HOME=/tmp/exocomp-dev ./bin/exocomp serve --web-addr 127.0.0.1:8642
```

Store the recovery kit `init` prints; it is the only copy of the recovery key.

With that data dir:
- The data dir is `/tmp/exocomp-dev/exocomp`; the single-instance lock is
  `<data dir>/serve.lock`, and the database initializes at `<data dir>/exocomp.db`.
- The web surface is TLS on `https://127.0.0.1:8642` with a self-signed
  certificate; add `--web-insecure-http` to serve it over plain HTTP.
- The token from `token new web` signs in.

## Web templates and Templ generation

The web interface uses [Templ](https://github.com/a-h/templ) for type-safe component rendering.

If you edit `.templ` files in `internal/web/`:

```bash
go run github.com/a-h/templ/cmd/templ@v0.3.906 generate
```

Commit the generated `*_templ.go` files alongside the `.templ` source files.

## Interacting with the local daemon via CLI

Run commands against your running development daemon. `--local` keeps them on this
machine when a default remote is configured:

```bash
XDG_DATA_HOME=/tmp/exocomp-dev ./bin/exocomp doctor --local
XDG_DATA_HOME=/tmp/exocomp-dev ./bin/exocomp source list --local
```

To reload schedules and transports after changing configuration:

```bash
XDG_DATA_HOME=/tmp/exocomp-dev ./bin/exocomp reload --local
```
