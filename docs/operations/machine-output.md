# Machine-readable output: `--output json`, the envelope, and exit codes

A structured-output contract (`internal/wire`) that the CLI, the HTTP API, and the MCP
server all marshal through, so a resource's bytes are identical whichever face asked.
Design rationale: [agent-interface.md §3.2](../design/agent-interface.md); ratified in
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

## `exocomp mcp`: the third face

`exocomp mcp` speaks the Model Context Protocol over stdio: every tool call and resource
read routes to the same `internal/service` verbs and marshals the same `wire` envelope the
CLI and the API use, so a resource's MCP bytes match its `--output json` and `/api/v1`
bytes. It runs at the local operator's authority (the process holds the data dir directly,
the same trust boundary a local `exocomp` invocation has) — there is no per-call scope gate
on stdio; a token-scoped HTTP/SSE transport is a follow-up, not shipped. Read tools cover
jobs/workspaces/drift/approvals/sources/schedules/rules/events/secrets(metadata)/backups/
health; `workspace_plan`/`workspace_apply` enqueue through the service exactly as the API
does; `approve_job`/`reject_job` call `reactive.Decide` server-side rather than ever
returning a decision link. `run_playbook` is deliberately not exposed yet — the inventory
and `--source` path resolvers it would need are still CLI-resident.

Related: [Remote CLI](remote-access.md) · [The API surface](api.md).
