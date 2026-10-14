---
type: Decision
title: "Decision 0014 — Agent-facing surface: one core, three thin programmatic faces"
description: "One core with three thin programmatic faces, the machine CLI, the HTTP API and MCP, sharing the one auth model, the one queue and the wire envelope."
status: stable
tags: [decision, api, mcp, cli]
---

# Decision 0014 — Agent-facing surface: one core, three thin programmatic faces

Status: Accepted · 2026-08-23

> exocomp maintains exactly one programmatic MCP surface — MCP over HTTP with a bearer token
> and the Decision 0009 scopes — and no ambient-authority MCP path. The MCP surface uses this
> uniform bearer token, never ambient local-operator authority.

## Context

exocomp's charter is operator-centric: a human runs the CLI, the TUI, or the web/phone
approval surface. The charter does not name an agent, an MCP client, or a remote CLI consumer
as a requirement. `docs/design/agent-interface.md` extends the charter to cover that gap
(design doc §7); this record makes agent-fit a first-class charter concern instead of an
assumption a design doc carries alone.

The concrete need is real and built: run homelab-ansible operations from an operator
workstation, over the tailnet, against a remote exocomp daemon — plan/apply a workspace, run a
playbook, watch a job, decide an approval — without SSHing in or screen-scraping aligned text.
The same surface works for a small, local model that branches on integers and typed codes
rather than parsing prose.

Two invariants bound the design space:

- **Invariant 5** — every execution path goes through the one job queue; guardrails live in
  the queue, not in configuration. A programmatic face that could enqueue by any other means
  would be the divergent path Decision 0001's thin-core decision rules out.
- **Invariant 3** — subprocess correctness is decided by exit codes only. A machine face
  gets the same signal a human gets from a shell, not a parallel truth.

exocomp exposes the core through three thin programmatic faces on one contract foundation: the
`internal/wire` envelope, `--output json` across CLI verbs, `doctor --output json`, read-only
`/api/v1`, cobra `Example` blocks, and `commands --output json`. The write half of `/api/v1`,
remote CLI client mode, and the HTTP MCP server extend that foundation; each passes security
review and the usual `make check` gates.

`/api/v1` and the HTTP MCP transport ride the Decision 0009 web listener (`internal/web`), not
`internal/api` — the separate loopback tofu state backend, which has no route-extension hook of
its own. The web mount seam and `internal/service` are the infrastructure that makes this
possible.

## Decision

**exocomp exposes its one operational core through three thin programmatic faces — CLI
`--output json`, an HTTP `/api/v1`, and an MCP-over-HTTP surface — that share one contract and
one execution path. No face is a second way to mutate state, and no face acts without a scoped
credential.**

1. **One core, thin faces.** Every face calls the same `internal/service` verbs; none reaches
   the store or the queue directly. `internal/service` imports queue/store/reactive/executor/
   secrets and never `net/http`, `cobra`, or `templ`. This is what makes invariant 5 hold for
   machine callers, not just human ones — a bug or a shortcut in one face cannot bypass a
   guardrail another face enforces.

2. **Every managed-state and execution write routes through the one queue.**
   `PlanWorkspace`/`ApplyWorkspace` enqueue via `queue.Enqueue`/`EnqueueGated`; nothing behind
   `/api/v1` or an MCP tool shells out or mutates state directly. A destructive call lands in
   `awaiting_approval` the same way a CLI invocation with `--require-approval` does — there is
   no faster, unguarded path for a machine caller than the one a human already has. The
   carve-out is narrow and not a side door: an *ephemeral control-plane* action — the
   drain/maintenance cordon that stops new schedule fires and new job claims ahead of a planned
   reboot — is an operator-scoped write that is deliberately NOT a queue job, because it toggles
   transient daemon state, not managed state or execution. It touches no workspace, runs no
   subprocess, and enqueues nothing (`internal/web/api_maintenance.go`). The invariant binds
   managed-state and execution writes to the queue; it does not turn every control-plane toggle
   into a job.

3. **One agent-aware MCP surface: HTTP + bearer + the Decision 0009 scopes.** exocomp maintains
   exactly one programmatic MCP surface — MCP over HTTP, mounted on the same Decision 0009 listener
   as `/api/v1` (`POST /mcp`, `internal/web/mcp_http.go`), behind the same `/api/v1` write
   guard, stateless so a revoke bites on the next call. It is not a second server: there is one
   MCP core — one tool/resource catalog, one set of handlers, one dispatch (`internal/mcp`) —
   and it calls the same in-process `internal/service` verbs `/api/v1` calls. The MCP-HTTP face
   is the agent-aware wrapper around the `/api/v1` bearer tokens: every MCP principal presents a
   scoped token, and the three Decision 0009 scopes map 1:1 onto the capabilities the catalog gates
   on — `readonly` / `operator` / `approve` are R / W / A (`internal/mcp/caller.go`).

   **Scope-gated, with W and A disjoint.** MCP tool visibility is scope-gated on the token: a
   `readonly` token advertises only read tools; an out-of-scope call is rejected before any
   store touch. The write and approve floors are DISJOINT — the `operator` scope that enqueues a
   gated apply cannot settle it, and no principal may decide an approval for a job its own token
   asked for. Approval is possible only with an explicitly minted `approve`-scope
   token; `Caller.Decide` is non-nil only under that scope and the decision seam is dropped for
   any other, so the disjointness holds at construction, not by every call site remembering it.
   There is no ambient approver anywhere on the surface.

   **The LOCAL path is the agent-aware CLI.** A local agent does not need an MCP server of its
   own: the CLI is already agent-aware — `--output json`, the exit-code table, and the same
   `internal/wire` envelope the HTTP faces marshal. A local harness drives exocomp through the
   CLI exactly as a human does, branching on codes rather than parsing prose.

   **stdio is not a maintained exocomp surface.** exocomp ships and maintains no stdio MCP
   server, and no `exocomp mcp` command. A harness that must speak stdio MCP runs an
   off-the-shelf http-to-stdio bridge (e.g. `npx mcp-remote` or `supergateway`) around
   `POST /mcp` with a bearer token; the bridge is the client's concern, not exocomp's code.

   **Why this is the design.** There is one operational core, and MCP is a thin token-scoped
   face over `/api/v1` — not a second server, and not an ambient-authority path. Keeping it to
   one credential-bearing HTTP surface is what makes "a revoked token cuts off every programmatic
   face" actually true: with no tokenless MCP path there is no face a revoke cannot reach.
   No ambient authority also closes the self-approval class structurally — no principal ever
   acts un-scoped, so an autonomous agent can never both raise a gate with `workspace_apply` and
   hold its release (the ambient-self-approval concern). The security property lives in "every
   principal presents a scoped token", not in policing two codebases.

4. **The `internal/wire` envelope is the one contract all three faces marshal through.**
   Every success and every failure is one shape:

   ```json
   { "schema_version": 1, "kind": "job.list", "data": [ … ] }
   { "schema_version": 1, "kind": "error",
     "error": { "code": "concurrency_held", "message": "…", "hint": "…" } }
   ```

   - **`schema_version`** is a pinned contract; a shape change bumps it. Prompt caches and
     small-model parsers depend on it not moving silently.
   - **Values-free, high-signal.** Following the drift-payload precedent (identifiers,
     states, keys, counts — never attribute values, ciphertext, or full stdout), every field
     in the envelope is safe to hand to a model with no redaction step of its own.
   - **Deterministic, cache-stable bytes.** Fixed sort order (store order, never map
     iteration), fixed RFC3339 UTC timestamps, `omitempty` throughout, and no volatile field
     (a relative "age", a "now") in a cacheable body. Unchanged state produces byte-identical
     output across repeated calls — what makes agent polling diffable and keeps a model's
     prompt cache warm instead of invalidating it every turn.
   - **Typed errors, RFC-9457-shaped.** `{code, message, hint}` replaces the flat
     `{"error": msg}` the loopback tofu backend uses today, mapped 1:1 onto the existing store
     and reactive sentinels (`not_found`, `conflict`, `duplicate`, `concurrency_held`,
     `stale_plan`, `kdf_busy`, `unauthenticated`, `forbidden`) so a caller branches on `code`,
     never on a sentence.
   - **A small, stable exit-code table** is the primary signal for the CLI face, codifying
     what invariant 3 already gives:

     | Code | Meaning |
     |---|---|
     | 0 | ok |
     | 1 | generic failure |
     | 2 | changes present (plan `-detailed-exitcode`) |
     | 3 | not found |
     | 4 | unauthenticated / forbidden |
     | 5 | conflict / concurrency held / locked |
     | 6 | awaiting approval |
     | 7 | busy — retry later |

     The JSON `error.code` and the process exit code are two views of the same taxonomy.

5. **Local-AI and cache-stability rationale is load-bearing, not incidental.** The design
   target is a weak, small-context model driving exocomp by branching on integers and typed
   codes — not a large model parsing aligned human text. That constrains the contract in ways
   a purely human-facing API would not need: values-free payloads (a small model should never
   see a secret to redact correctly), stable byte-for-byte output on unchanged state (so a
   local model's prompt cache stays warm across polls instead of recomputing from scratch),
   and a closed, small error/exit-code vocabulary (so a weak model can dispatch on a handful
   of codes instead of pattern-matching prose).

6. **The full surface is implemented.** `internal/service` is the verb layer: the CLI,
   `/api/v1`, and MCP all call it, and none reaches the store or queue directly
   (`internal/service/*.go`). The write half of `/api/v1` — plan/apply/run/decide,
   idempotency-keyed and namespaced per token + route, gated — lives in
   `internal/web/api_write.go`. Remote CLI client mode — `exocomp login` (TOFU cert pinning),
   the `--remote` / `EXOCOMP_REMOTE` selector, and `internal/client` as the typed HTTP client
   (`cmd/exocomp/login.go`, `cmd/exocomp/remote.go`, `internal/client/*.go`) — lets an operator
   workstation drive a remote daemon over the tailnet. The selector is `--remote`, not
   `--host`: `--host` is already the ansible target pattern (`exocomp command --host
   <pattern>`) and reusing it for the daemon would collide. The HTTP MCP server (`internal/mcp`
   dispatch mounted at `POST /mcp` on the Decision 0009 listener, `internal/web/mcp_http.go`)
   completes the set. All three pass the same security review and `make check` gates as the
   rest of the surface.

## Consequences

- Agent-fit is a first-class charter concern, not an assumption implicit in a design doc —
  future work on the CLI, `/api/v1`, or MCP cites this record instead of re-litigating whether a
  programmatic surface belongs here at all.
- The three faces converge on one marshaller and one set of golden tests: a CLI JSON shape and
  an API JSON shape are the same commitment, not two contracts that can drift apart.
- `internal/service` is the one verb layer the write API and MCP faces call; the CLI,
  `/api/v1`, and MCP share it rather than each reimplementing a verb.
- **A revoked token cuts off every programmatic face at once.** Every programmatic face is
  token-bearing: the remote CLI (`--remote`), `/api/v1`, and MCP-HTTP each present an Decision 0009
  bearer, each is stateless and re-resolves the credential per call, and there is no tokenless
  MCP path for a revoke to miss. A revoke bites on the next request on every face, not at
  session end — there is no ambient-authority path it could fail to reach, because exocomp
  maintains no such server; a stdio client bridges to `POST /mcp` and carries the same bearer.
- exocomp has no ambient MCP authority, which closes the self-approval class structurally: no
  principal ever acts un-scoped, W and A are disjoint, and approval requires an explicitly
  minted `approve` token, so a face that raises a gate can never also settle it.
- One follow-on is deferred, not a blocker: OAuth 2.1 for the MCP-HTTP transport. The seam
  exists (the transport already resolves a bearer per request through the hardened `/api/v1`
  path), but token-issuance flows beyond Decision 0009 minting are not built; a harness that wants
  OAuth today is served by the bearer it already carries.
- The exit-code table and typed-error taxonomy are a stable contract: a future change to
  either bumps `schema_version`, the same discipline the envelope shape already follows.

See `docs/design/agent-interface.md` for the full design rationale.
