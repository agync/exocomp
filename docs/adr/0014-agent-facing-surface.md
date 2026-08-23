# ADR 0014 — Agent-facing surface: one core, three thin programmatic faces

Status: Proposed · 2026-08-23

> **The open design question is now RESOLVED.** The one governance question this ADR carried —
> whether the MCP surface keeps ambient local-operator authority or moves to a uniform ADR-0009
> token — is settled: exocomp maintains exactly one programmatic MCP surface, MCP over HTTP with
> a bearer token and the ADR-0009 scopes, and no ambient-authority MCP path. The surface this
> ADR describes is built and hardened; the only remaining work is the stdio-server removal this
> document ratifies. The ADR is ready to ratify at the owner's discretion.

## Context

exocomp's charter is operator-centric: a human runs the CLI, the TUI, or the web/phone
approval surface. Nothing in the charter names an agent, an MCP client, or a remote CLI
consumer as a requirement, and `docs/design/agent-interface.md` exists precisely because that
gap was found by a design pass, not asked for by the charter. Ratifying agent-fit as a
first-class concern — rather than leaving it as an unratified design doc — is the decision
this ADR records (design doc §7).

The concrete need is real and now built: run homelab-ansible operations from an operator
workstation, over the tailnet, against a remote exocomp daemon — plan/apply a workspace, run a
playbook, watch a job, decide an approval — without SSHing in or screen-scraping aligned text.
The same surface must work for a small, local model that branches on integers and typed codes
rather than parsing prose.

Two invariants bound the design space:

- **Invariant 5** — every execution path goes through the one job queue; guardrails live in
  the queue, not in configuration. A programmatic face that could enqueue by any other means
  would be the divergent path ADR-0001's thin-core decision rules out.
- **Invariant 3** — subprocess correctness is decided by exit codes only. A machine face
  should get the same signal a human gets from a shell, not a parallel truth.

The whole surface has since shipped and been hardened. The contract foundation landed first
(`internal/wire`, `--output json`, `doctor --output json`, read-only `/api/v1`, cobra
`Example` blocks, `commands --output json`), and the session this ADR is reconciled against
landed the rest: the `internal/service` verb layer, the write `/api/v1`, remote CLI client
mode, and the HTTP MCP server — all through the red-team wave that closed findings
AI-1..AI-13. This ADR therefore ratifies shipped reality, not a direction; the build-status
detail is in Decision point 6, and the one code change that remains is the removal of the
bespoke stdio MCP server (Decision point 3).

**On a correction the design doc's verification pass raised.** The design doc originally
described `internal/api`'s `Mount(routes)` seam as something `/api/v1` could ride directly on
`internal/web`. It could not: `internal/web/server.go` had no route-extension hook of its own —
that seam existed only on `internal/api`, the separate loopback tofu state backend. The web
mount seam and `internal/service` were *built* as part of this work, not reused as
pre-existing infrastructure — and they now exist, so `/api/v1` and the HTTP-MCP transport ride
the ADR-0009 web listener as the target described.

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

3. **One agent-aware MCP surface: HTTP + bearer + the ADR-0009 scopes.** exocomp maintains
   exactly one programmatic MCP surface — MCP over HTTP, mounted on the same ADR-0009 listener
   as `/api/v1` (`POST /mcp`, `internal/web/mcp_http.go`), behind the same `/api/v1` write
   guard, stateless so a revoke bites on the next call. It is not a second server: there is one
   MCP core — one tool/resource catalog, one set of handlers, one dispatch (`internal/mcp`) —
   and it calls the same in-process `internal/service` verbs `/api/v1` calls. The MCP-HTTP face
   is literally the agent-aware wrapper around the `/api/v1` bearer tokens: every MCP principal
   presents a scoped token, and the three ADR-0009 scopes map 1:1 onto the capabilities the
   catalog gates on — `readonly` / `operator` / `approve` are R / W / A (`internal/mcp/caller.go`).

   **Scope-gated, with W and A disjoint.** MCP tool visibility is scope-gated on the token: a
   `readonly` token advertises only read tools; an out-of-scope call is rejected before any
   store touch. The write and approve floors are DISJOINT — the `operator` scope that enqueues a
   gated apply cannot settle it, and no principal may decide an approval for a job its own token
   asked for (finding AI-2). Approval is possible only with an explicitly minted `approve`-scope
   token; `Caller.Decide` is non-nil only under that scope and the decision seam is dropped for
   any other, so the disjointness holds at construction, not by every call site remembering it.
   There is no ambient approver anywhere on the surface.

   **The LOCAL path is the agent-aware CLI.** A local agent does not need an MCP server of its
   own: the CLI is already agent-aware — `--output json`, the ratified exit-code table, and the
   same `internal/wire` envelope the HTTP faces marshal. A local harness drives exocomp through
   the CLI exactly as a human does, branching on codes rather than parsing prose.

   **stdio is not a maintained exocomp surface.** exocomp ships and maintains no stdio MCP
   server. A harness that must speak stdio MCP runs an off-the-shelf http-to-stdio bridge (e.g.
   `npx mcp-remote` or `supergateway`) around `POST /mcp` with a bearer token; the bridge is the
   client's concern, not exocomp's code. The bespoke stdio server that shipped earlier this
   session (`exocomp mcp`) is being REMOVED in a follow-on code change, and this ADR ratifies
   that end-state.

   **Why this is the design.** There is one operational core, and MCP is a thin token-scoped
   face over `/api/v1` — not a second server, and not an ambient-authority path. Keeping it to
   one credential-bearing HTTP surface is what makes "a revoked token cuts off every programmatic
   face" actually true: with no tokenless MCP path there is no face a revoke cannot reach.
   Removing ambient authority also closes the self-approval class structurally — no principal
   ever acts un-scoped, so an autonomous agent can never both raise a gate with `workspace_apply`
   and hold its release (the ambient-self-approval concern behind finding AI-1). The security
   property lives in "every principal presents a scoped token", not in policing two codebases.

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
   - **A small, ratified exit-code table** is the primary signal for the CLI face, codifying
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

6. **The surface is built — this ADR ratifies shipped reality, not a promise.** The contract
   foundation landed first (`internal/wire` envelope and schema, `--output json` on
   read/list/status CLI verbs, `doctor --output json`, the read-only `/api/v1` endpoints on the
   ADR-0009 listener, cobra `Example` blocks on every command, and `commands --output json`),
   and the rest — everything the earlier draft of this ADR listed as "proposed and unbuilt" —
   has since shipped:

   - `internal/service` is extracted and real (`internal/service/*.go`): the verb layer the
     CLI, `/api/v1`, and MCP all call, none reaching the store or queue directly.
   - the write half of `/api/v1` (plan/apply/run/decide, idempotency-keyed and namespaced per
     token + route, gated) — `internal/web/api_write.go`.
   - remote CLI client mode — `exocomp login` (TOFU cert pinning) and the `--remote` /
     `EXOCOMP_REMOTE` selector, with `internal/client` as the typed HTTP client
     (`cmd/exocomp/login.go`, `cmd/exocomp/remote.go`, `internal/client/*.go`). The selector is
     `--remote`, NOT `--host`: `--host` is already the ansible target pattern
     (`exocomp command --host <pattern>`) and reusing it for the daemon would collide.
   - the HTTP MCP server — `internal/mcp` dispatch mounted at `POST /mcp` on the ADR-0009
     listener (`internal/web/mcp_http.go`).

   All of it passed through the red-team wave that closed findings AI-1..AI-13 (2026-08-23).
   The one code change that remains is the REMOVAL of the bespoke stdio MCP server (`exocomp
   mcp`, Decision point 3); this ADR ratifies that end-state — a single MCP surface, HTTP and
   token-scoped, with an off-the-shelf bridge for any stdio client. Beyond that removal the
   ADR records the direction the shipped code took; it no longer scopes unbuilt work.

## Consequences

- Agent-fit becomes a charter concern an operator has explicitly ratified, not an assumption
  a design doc smuggled in — future work on the CLI, `/api/v1`, or MCP can cite this ADR
  instead of re-litigating whether a programmatic surface belongs here at all.
- The three faces are now bound to converge on one marshaller and one set of golden tests; a
  CLI JSON shape and an API JSON shape are the same commitment, not two contracts that can
  drift apart.
- `internal/service` extraction was the prerequisite for the write API and MCP faces — both
  needed one verb layer to call without forking logic — and it now exists, so the CLI,
  `/api/v1`, and MCP share it rather than each reimplementing a verb.
- **A revoked token cuts off every programmatic face at once — now true without caveat.** Every
  programmatic face is token-bearing: the remote CLI (`--remote`), `/api/v1`, and MCP-HTTP each
  present an ADR-0009 bearer, each is stateless and re-resolves the credential per call, and
  there is no tokenless MCP path for a revoke to miss. So a revoke bites on the next request on
  every face, not at session end. The old exception — a stdio server running at ambient
  authority — is gone by construction, because exocomp maintains no such server; a stdio client
  bridges to `POST /mcp` and carries the same bearer.
- Removing ambient MCP authority closes the ambient-self-approval class structurally: no
  principal ever acts un-scoped, W and A are disjoint, and approval requires an explicitly
  minted `approve` token, so a face that raises a gate can never also settle it (finding AI-1,
  AI-2).
- One follow-on is deferred, not a blocker: OAuth 2.1 for the MCP-HTTP transport. The seam
  exists (the transport already resolves a bearer per request through the hardened `/api/v1`
  path), but token-issuance flows beyond ADR-0009 minting are not built; a harness that wants
  OAuth today is served by the bearer it already carries.
- The exit-code table and typed-error taxonomy are now a stable contract: a future change to
  either bumps `schema_version`, the same discipline already ratified for the envelope shape.
- Remote CLI client mode, the write API, and the HTTP MCP server have shipped and been hardened
  (each gated by the usual `make check` and, per invariant 8, docs moving with the code). The
  open design question — a single MCP+HTTP surface with no ambient authority — is RESOLVED, so
  this ADR is an accurate account of a built surface and is ready to ratify at the owner's
  discretion. The only work it still ratifies rather than records is the stdio-server removal
  (Decision point 3).

See `docs/design/agent-interface.md` for the full proposal, the phased build plan, and the
evidence trail this ADR ratifies the direction of.
