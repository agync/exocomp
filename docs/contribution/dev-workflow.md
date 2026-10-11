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

Run `exocomp serve` pointing to an isolated development data directory:

```bash
mkdir -p /tmp/exocomp-dev
./bin/exocomp serve --data-dir /tmp/exocomp-dev --listen 127.0.0.1:8642
```

In development mode:
- The single-instance lock file is held at `/tmp/exocomp-dev/serve.lock`.
- SQLite database initializes at `/tmp/exocomp-dev/exocomp.db`.
- The Web UI is immediately accessible at `http://127.0.0.1:8642`.

## Web templates and Templ generation

The web interface uses [Templ](https://github.com/a-h/templ) for type-safe component rendering.

If you edit `.templ` files in `internal/web/`:

```bash
go run github.com/a-h/templ/cmd/templ@v0.3.906 generate
```

Commit the generated `*_templ.go` files alongside the `.templ` source files.

## Interacting with the local daemon via CLI

Run commands against your running development daemon:

```bash
./bin/exocomp --data-dir /tmp/exocomp-dev doctor
./bin/exocomp --data-dir /tmp/exocomp-dev source list
```

To reload schedules and transports after changing configuration:

```bash
./bin/exocomp --data-dir /tmp/exocomp-dev reload
```
