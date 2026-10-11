---
type: Guide
title: "Developer environment setup"
description: "Setting up a local workstation to build, test, and develop exocomp: toolchain, sandbox, and git hooks."
status: stable
tags: [contributing, dev, setup, toolchain, hooks]
---

# Developer environment setup

exocomp compiles to a single static Go binary (`exocompd` / `exocomp`) with zero external C library dependencies.

## Prerequisites

1. **Go toolchain**: Go 1.23 or newer.
2. **Container runtime**: Rootless Podman 5.0+ (or Docker 26+).
3. **Execution tools**:
   - `ansible-core` (>= 2.17)
   - `opentofu` (>= 1.8)
   - `restic` (>= 0.17)
   - `age` (>= 1.2)
   - `sops` (>= 3.9)
   - `uv` (>= 0.4)
4. **Sandboxing (Linux)**: `nono` installed on `PATH` for the `enforcing` sandbox tier.

## Installing Git hooks

The repository enforces pre-commit, pre-push, and commit-msg quality gates via `pre-commit`:

```bash
pip install pre-commit
make hooks
```

This wires:
- **`pre-commit`**: Automatically runs `make check` (formatting, vet, lint, tidy, `go test -short`).
- **`commit-msg`**: Rejects titles longer than 52 characters, non-conventional formats, or commit trailers (Invariant 9).
- **`pre-push`**: Runs the full `make check-race` suite.

## Building the binary

Compile the local binary into `bin/exocomp`:

```bash
make build
```

Verify build output and embedded version metadata:

```bash
./bin/exocomp version
```
