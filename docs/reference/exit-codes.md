---
type: Reference
title: "Exit codes and error taxonomy"
description: "Process exit code mappings, failure domains, and machine-readable error categories."
status: stable
tags: [reference, exitcodes, errors, failures]
---

# Exit codes and error taxonomy

Invariant 3 states: **Subprocess correctness is decided by exit codes only; parsed output enriches the UI**.

exocomp maps subprocess and daemon failure states to deterministic exit codes.

## Process exit codes

| Code | Status | Meaning |
|---|---|---|
| `0` | Success | Command completed cleanly with zero errors |
| `1` | Failure | Unhandled tool error or general execution failure |
| `2` | Refused | Safety guardrail or invariant violation prevented execution |
| `3` | Degraded | Run completed but tolerated offline/unreachable nodes |
| `4` | Drifting | Check mode (`--check`) detected configuration drift |
| `5` | Cordoned | Daemon is in drain mode; new jobs refused |
| `6` | Locked | Another daemon instance holds the database/lockfile |

## Error domains in JSON envelopes

When invoked with `--output json`, error responses categorize failures under `error.domain`:

- `auth`: Bearer token invalid, revoked, or missing required scope.
- `policy`: CEL policy rejected an OpenTofu plan or event action.
- `sandbox`: Path access denied by the `nono` driver.
- `crypto`: Envelope decryption failed or age identity is invalid.
- `store`: SQLite concurrency conflict or schema migration error.
