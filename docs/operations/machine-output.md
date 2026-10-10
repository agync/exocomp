---
type: Reference
title: "Machine-readable output"
description: "The json output envelope, the error taxonomy, the exit-code table and what of it reaches the process today, and MCP over HTTP as the third face."
status: stable
tags: [operations, cli, json]
---

# Machine-readable output: `--output json`, the envelope, and exit codes

A structured-output contract (`internal/wire`) that the CLI, the HTTP API, and the MCP
server all marshal through, so a resource's bytes are identical whichever face asked.
Design rationale: [agent-interface.md §3.2](../design/agent-interface.md); recorded in
[ADR 0014](../adr/0014-agent-facing-surface.md).

## Selecting JSON

```
exocomp jobs list --output json
EXOCOMP_OUTPUT=json exocomp doctor
```

Precedence: an explicit `--output` flag wins, then `EXOCOMP_OUTPUT`, then the `text`
default. The flag is registered once, globally, and every command reads it back the same
way. `--output` accepts only `text` or `json`; anything else is a flag-parse error.

Read/list/status verbs support it; every one carries a cobra `Example` showing the JSON
form. `exocomp commands --output json` dumps the whole command tree (name, path, short
description, example, flags, subcommands) so a harness discovers the CLI's surface in one
call without scraping `--help`.

Coverage is now total across every read/list/status verb; the last five gaps closed are
`mute list` (`mute.list`), `litestream status` (`litestream.status`), `seal status`
(`seal.status`), `handoff status` (`workflow.status`), and `hooks show` (`hooks.show`). Each
follows the values-free discipline below: the text default is unchanged, and no secret
value, ciphertext, or raw credential-bearing target URI ever reaches the JSON — `seal
status` reports the transit key's address and name but never its token; `litestream status`
reports a target's type but never its raw URI, which for `sftp://` can embed `user:pass`.

## The envelope

```json
{ "schema_version": 1, "kind": "job.list", "data": [ … ] }
{ "schema_version": 1, "kind": "error",
  "error": { "code": "not_found", "message": "…", "hint": "check the id or name; list the collection to see what exists" } }
```

- **`schema_version`** is a pinned contract (currently `1`); a shape change bumps it.
- **`kind`** names the payload (`job.list`, `job.get`, `workspace.list`, `doctor`, …) so a
  caller dispatches without guessing.
- **`data`** has no `omitempty`: an empty list still prints `[]`, never vanishes.
- Struct fields marshal in declaration order and HTML escaping is off, so unchanged state
  produces byte-identical bytes across repeated calls — what keeps a small local model's
  polling loop cache-stable.
- Every projection type is values-free: identifiers, states, keys, and counts only. A
  `SecretView` has no field a ciphertext or nonce could occupy; a job view carries no
  operator-supplied `Args`.

## The error taxonomy

Every JSON `error.code` maps 1:1 onto a real sentinel — no code is invented per endpoint:

| `code` | Sentinel |
|---|---|
| `not_found` | `store.ErrNotFound` |
| `conflict` | `store.ErrStateConflict` |
| `duplicate` | `store.ErrDuplicate` |
| `concurrency_held` | `store.ErrConcurrencyHeld` |
| `stale_plan` | `reactive.ErrStalePlan` |
| `kdf_busy` | the web auth gate is saturated (transient; retry) |
| `streams_over_cap` | the web stream boundary refuses an over-cap SSE attach (transient; retry) |
| `unauthenticated` / `forbidden` | the web/API auth boundary |
| `invalid_argument` | a malformed request parameter (API/MCP only) |
| `internal` | anything unmatched |

Each carries a stable, code-keyed `hint` with no volatile detail, so it stays cache-friendly
across repeated calls too.

## Exit codes: the defined table, and what reaches the process

`internal/wire` defines a small exit-code table as the primary machine signal (codifying
invariant 3 — subprocess correctness is exit-code-only):

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | generic failure |
| 2 | changes present (`tofu plan -detailed-exitcode`) |
| 3 | not found |
| 4 | unauthenticated / forbidden |
| 5 | conflict / concurrency held / locked |
| 6 | awaiting approval |
| 7 | busy — retry later |

Two independent things produce this exit code, and both are wired.

`cmd/exocomp/main.go`'s `Execute()` maps any error a command returns onto the table through
`exitCodeFor` (`cmd/exocomp/exitcode.go`): a local error carrying a store/reactive sentinel
resolves via `wire.ExitFor`, a remote failure (`client.APIError`, from a daemon's wire error
envelope or a bare non-2xx status) resolves via its own `ExitCode()` — the same
`wire.ExitForCode` table either way, so `jobs get <missing id>` exits `3` whether the id was
missing locally or on a remote daemon, and a `403` exits `4` the same way. An error that
matches no taxonomy sentinel (a plain `errors.New`, a malformed-flag error) exits `1`,
unchanged from before this mapping existed.

Code `2`, and the "partial" outcomes non-zero exit codes for a job's own run outcome, are
produced separately: specific commands (`workspace plan`/`apply` on changes-present,
`playbook run`, `command`/`ansible`, `playbook vendor`) call `os.Exit` directly with the
job's own exit code once the store and secrets envelope are released — a `cobra.RunE`
cannot carry an exit code, so those calls bypass `Execute()`'s returned error, and
`exitCodeFor` never runs for them. That is deliberate, not a gap: it is the run's own outcome
(what the subprocess job did), not a taxonomy-classified request failure.

`6` (awaiting approval) is defined in the table but has no error sentinel that produces it: a
gated write is a *successful* enqueue — the job lands in `awaiting_approval` and the CLI
prints the pending approval and exits `0` (`ApprovalPending`/`approval.pending`, not an
`error`). An agent tells a gated write apart from a clean one by the response `kind`
(`job.enqueued` vs `approval.pending`), not by exit code.

## `doctor --output json`

`doctor` without a `--remote` runs `internal/health.Probe` against the local data dir and
emits `kind: "doctor"`, a `HealthView`: a fixed-order list of `{name, status, detail}`
entries, `status` one of `ok | warn | fail | unknown`. It never short-circuits on a data-dir
fault the way the text report does — an agent gets every subsystem's status in one call, the
fault carried per-entry rather than aborting the probe. `GET /api/v1/health` and `doctor
--remote <host>` call the identical probe, so the three surfaces cannot diverge. See
[Remote CLI](remote-access.md) for the remote form.

The `optional_tools` entry also folds in the toolchain-manifest comparison: each resolved
tool version is diffed against its pin in the manifest named by `EXOCOMP_TOOLCHAIN_MANIFEST`.
A tool off its pin is `warn` — never `fail`, because the [ADR 0006](../adr/0006-uv-toolchain-escape-hatch.md)
operator escape hatch is a supported state; a version the extraction rule cannot parse is
named in the detail without moving the status; and no manifest env at all leaves the entry
`unknown`, a state to read rather than a fault. The text report marks each divergent line
against its pin (`restic 0.19.1 — pinned 0.19.0`) and prints the active manifest path once.
The same entry carries the deployment's recorded image ref (`image-ref: <ref>`, or
`image-ref: not recorded`) — the value `RECOVERY.md` names, so a recovered host knows what
image to load.

## MCP-over-HTTP: the third face

exocomp maintains exactly one agent MCP surface: `POST /mcp` on the ADR-0009 listener, the
same one `/api/v1` rides. It runs one catalog and one set of handlers, so every tool call and
resource read routes to the same `internal/service` verbs and marshals the same `wire`
envelope the CLI and the API use — a resource's MCP bytes match its `--output json` and
`/api/v1` bytes.

The transport authenticates with a bearer token and filters both `tools/list` and every
`tools/call` by its scope; there is no ambient or unauthenticated path onto it. Read tools
cover jobs/workspaces/drift/approvals/sources/schedules/rules/events/secrets(metadata)/
backups/health; `workspace_plan`/`workspace_apply` enqueue through the service exactly as the
API does; `approve_job`/`reject_job` exist only for an `approve`-scope token, and call
`reactive.Decide` server-side rather than ever returning a decision link. `run_playbook` is
deliberately not exposed yet — the inventory and `--source` path resolvers it would need are
still CLI-resident.

exocomp ships no stdio MCP server. A local agent working the filesystem alongside exocomp is
the sanctioned LOCAL path and drives the agent-aware CLI directly (`--output json`, the
exit-code table above) — no MCP hop needed. A harness that only speaks stdio MCP bridges to
`POST /mcp` with an off-the-shelf http-to-stdio wrapper (`npx mcp-remote` or `supergateway`),
carrying the same bearer; the bridge is the client's concern, not exocomp's code.

Full connection details, the per-scope catalog, and the transport's shape:
[The MCP surface](mcp.md).

Related: [The MCP surface](mcp.md) · [Remote CLI](remote-access.md) ·
[The API surface](api.md).
