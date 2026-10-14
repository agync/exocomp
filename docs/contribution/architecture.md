---
type: Reference
title: "Codebase architecture map"
description: "Package relationships, dependency boundaries, the step ledger, and the nono execution driver."
status: stable
tags: [contributing, architecture, packages, internals]
---

# Codebase architecture map

exocomp implements a **thin-core architecture** ([Decision 0001](../decisions/0001-thin-core-architecture.md)): one rootless Go process that owns state, sequencing, keys, and recovery, delegating all external work to pinned CLI subprocesses.

## Package layout

```
cmd/exocomp/       The single binary entrypoint (alias exocompd)
internal/
├── store/         SQLite storage engine, migrations, and query interfaces
├── queue/         The single FIFO/priority job queue and runner dispatch
├── executor/      Subprocess drivers (disabled, enforcing with nono, container)
├── secrets/       Root age DEK envelope, HKDF subkeys, and RAM-backed staging
├── reactive/      CEL event rule evaluation engine and webhook dispatcher
├── service/       Lifecycle daemon orchestrator (serve, reload, cordon, drain)
├── api/           HTTP REST endpoints (/api/v1) and state backend handlers
├── mcp/           Model Context Protocol server for AI coding assistants
├── web/           Server-rendered HTML templates (Templ), assets, and tokens
├── tui/           Bubble Tea / Lip Gloss terminal interface
├── litestream/    Embedded SQLite replication and recovery manager
├── naming/        Shared identifier types and constants
└── wire/          Public API request/response wire types
```

## Architectural rules and dependency boundaries

1. **The store package is strictly unidirectional**:
   `internal/store` imports only `internal/naming`. It never imports `reactive`, `queue`, or `service`. Domain attributes (such as job kind, status, or artifact type) are passed to store methods as scalar strings to prevent circular import graphs.
2. **One queue, one execution path**:
   No subsystem ever executes a subprocess directly. All work (Ansible playbooks, OpenTofu commands, backup runs, ad-hoc commands) is enqueued as a job row in the SQLite step ledger.
3. **The sandbox driver (`nono`)**:
   Under the `enforcing` tier (Linux default), every job process is restricted by `nono`:
   - Access to the database file (`exocomp.db`) is denied.
   - Access to the keyring (`keyring.age`) and host keys is denied.
   - Access to the container runtime socket is denied.
   - Network egress is blocked unless `--privileged` is explicitly granted.
