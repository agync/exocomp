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

Process exit codes provide deterministic signals for automation and agents (Invariant 3).

| Code | Constant | Meaning | Source Sentinels / Context |
|---|---|---|---|
| `0` | `ExitOK` | Success | Clean completion, zero errors |
| `1` | `ExitFailure` | Generic failure | Run settled failed, tool error, or request failure |
| `2` | `ExitChangesPresent` | Changes present | OpenTofu plan detected changes (`-detailed-exitcode 2`) |
| `3` | `ExitNotFound` | Not found | Entity does not exist (`store.ErrNotFound`) |
| `4` | `ExitAuth` | Auth failure | Unauthenticated (`CodeUnauthenticated`) or forbidden (`CodeForbidden`) |
| `5` | `ExitConflict` | Conflict / Concurrency | State conflict (`ErrStateConflict`), lock held (`ErrConcurrencyHeld`), stale plan (`ErrStalePlan`) |
| `6` | `ExitAwaitingApproval` | Awaiting approval | Gated write or mutation landed pending human sign-off |
| `7` | `ExitBusy` | Transient busy | Auth KDF saturated (`CodeKDFBusy`) or SSE stream cap reached (`CodeStreamsOverCap`) |

## Error envelopes and code taxonomy

When invoked with `--output json`, failures are emitted as RFC 9457-shaped envelopes with a top-level `kind: "error"` and a schema version:

```json
{
  "schema_version": 1,
  "kind": "error",
  "error": {
    "code": "concurrency_held",
    "message": "workspace 'network' is locked by job_01j7abc...",
    "hint": "another run holds this workspace; wait for it to finish or cancel it"
  }
}
```

The error codes map 1:1 onto internal store, reactive, and auth sentinels:

| Error Code | Sentinel / Condition | Actionable Hint |
|---|---|---|
| `not_found` | `store.ErrNotFound` | Check the ID or name; list the collection to see what exists |
| `conflict` | `store.ErrStateConflict` | Re-read entity state; the transition requested is not legal |
| `duplicate` | `store.ErrDuplicate` | Name or key is already taken; choose another or remove existing |
| `concurrency_held` | `store.ErrConcurrencyHeld` | Another run holds this workspace; wait for it to finish or cancel it |
| `stale_plan` | `reactive.ErrStalePlan` | Workspace moved since plan was saved; re-plan before applying |
| `kdf_busy` | `web.ErrKDFBusy` | Auth gate is saturated; retry shortly |
| `streams_over_cap` | `CodeStreamsOverCap` | Too many live streams; close a tab or retry shortly |
| `unauthenticated` | `web.ErrUnauthenticated` | Supply a valid token or session |
| `forbidden` | `CodeForbidden` | Token scope is too low for the verb; mint one with higher scope |
| `internal` | `CodeInternal` | Fallback for unexpected internal errors |
