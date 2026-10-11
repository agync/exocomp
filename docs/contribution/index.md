---
type: Hub
title: "Contributor guide"
description: "How work lands in exocomp: developer setup, workflow, verification gates, worktree lanes, and conventions."
status: stable
tags: [contributing, dev, workflow, contract, invariants]
---

# Contributor guide

How work lands in exocomp and what gates it. The foundational contract lives in [AGENTS.md](../../AGENTS.md); this section provides the concrete developer manual and procedures.

## Developer guides

| Guide | Covers |
|---|---|
| [Developer setup](dev-setup.md) | Prerequisites (Go 1.23+, rootless Podman, `nono` sandbox) and git hooks activation |
| [Developer workflow](dev-workflow.md) | Running `exocompd` in development mode, `templ` template generation, and local CLI testing |
| [Testing and verification gates](testing.md) | `make check` fast loop, `make check-race`, mutation testing of safety guards, and Lima VM testbeds |
| [Codebase architecture map](architecture.md) | Package relationships, unidirectional store boundaries, the step ledger, and the nono execution driver |
| [Worktree lanes & branch hygiene](worktrees.md) | Parallel work in `.worktrees/`, linear commit history on `main`, and commit message conventions |
| [Schema migrations](migrations.md) | Authoring forward-only SQLite migrations, `PRAGMA user_version`, and collision resolution |

## The core contract (Invariants summary)

Every contributor (human and automated agent) adheres strictly to the invariants:
1. **One operator, one container, no external database**: SQLite with WAL mode only.
2. **Standard storage formats**: Plain SQLite, plain Restic repositories, plain Git bundles, standard age envelopes.
3. **Subprocess correctness**: Exit codes alone decide success; stdout/stderr streams enrich the UI.
4. **Zero secret leakage**: Secrets never appear in logs, job events, docs, commits, or test fixtures.
5. **One queue, one execution path**: All work runs through the queue; guardrails live in the ledger, not configuration.
6. **No sudo**: Work needing elevation becomes an explicit request to the operator.
7. **Disaster recovery independence**: DR never depends on a registry, vendor, or this project's code.
8. **Docs move with code**: `docs/` is an OKF v0.2 bundle updated in the same commit series as the code it describes.
9. **Commit hygiene**: Logical units, title <= 52 characters, conventional format, no trailers, explicit file staging only.
10. **No AI slop**: Comments only for constraints the code cannot show; no filler prose.
11. **Parallel work in `.worktrees/`**: `main` remains linear (rebase then fast-forward).
12. **Automation over manual conformance**: Automated hooks gate commits (`make check`) and merges (`make check-race`).
13. **Agent scratch in `.agents/brain/`**: Ephemeral scratch only in `/tmp`; working docs live in the session brain.
