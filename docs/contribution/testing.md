---
type: Guide
title: "Testing and verification gates"
description: "Testing philosophy, fast vs race gates, mutation testing of safety guards, and Lima VM integration."
status: stable
tags: [contributing, testing, verification, gates, lima]
---

# Testing and verification gates

Verification in exocomp is grounded in execution. No change lands without demonstrated test output.

## The testing gates

### 1. Fast commit gate (`make check`)

Runs automatically via git pre-commit hook or manually:

```bash
make check
```

Executes:
- `gofmt` formatting check
- `go vet` static analysis
- `golangci-lint` (if installed)
- `go mod tidy -diff` check
- Fast unit tests: `go test -short ./...`

### 2. Full race gate (`make check-race`)

Runs automatically on `pre-push` and gates all worktree merges to `main`:

```bash
make check-race
```

Runs the entire test suite with Go's race detector (`go test -race ./...`).

## Mutation testing of safety guards

Invariant 5 dictates that guardrails live in the execution queue, not configuration.

To prevent tests from passing vacuously:
- Every safety guard test must carry a mutation check.
- A test asserting that destructive actions or unauthenticated tokens are rejected must fail if the security check in the code under test is commented out.

## Infrastructure testing with Lima Linux VMs

Mocking remote SSH and Ansible runs in unit tests cannot prove behavior on real Linux kernels.

The repository includes Fedora Lima VM templates under `test/lima/`:

```bash
# Provision a dummy managed node
limactl start test/lima/fedora.yaml

# Run integration tests against real SSH/Ansible targets
go test -tags=integration ./test/lima/...
```

This validates:
- Real SSH fleet key authentication and `known_hosts` fingerprinting.
- Rootless Ansible execution with `nono` sandbox enforcement.
- Per-source venv isolation and Python interpreter resolution.
