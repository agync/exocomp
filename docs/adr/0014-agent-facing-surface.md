# ADR 0014 — Agent-facing surface: one core, three thin programmatic faces

Status: Proposed · 2026-08-23

## Context

exocomp's charter is operator-centric: a human runs the CLI, the TUI, or the web/phone
approval surface. Nothing in the charter names an agent, an MCP client, or a remote CLI
consumer as a requirement, and `docs/design/agent-interface.md` exists precisely because that
gap was found by a design pass, not asked for by the charter. Ratifying agent-fit as a
first-class concern — rather than leaving it as an unratified design doc — is the decision
this ADR records (design doc §7).

The concrete need is real and already partly built: run homelab-ansible operations from an
operator workstation, over the tailnet, against a remote exocomp daemon — plan/apply a
workspace, run a playbook, watch a job, decide an approval — without SSHing in or
screen-scraping aligned text. The same surface must work for a small, local model that
branches on integers and typed codes rather than parsing prose.

Two invariants bound the design space:

- **Invariant 5** — every execution path goes through the one job queue; guardrails live in
  the queue, not in configuration. A programmatic face that could enqueue by any other means
  would be the divergent path ADR-0001's thin-core decision rules out.
- **Invariant 3** — subprocess correctness is decided by exit codes only. A machine face
  should get the same signal a human gets from a shell, not a parallel truth.

Work toward this has already shipped ahead of ratification: `internal/wire` (the envelope
package), `--output json` on read/list/status CLI verbs, `doctor --output json`, read-only
`/api/v1` JSON endpoints on the ADR-0009 listener, cobra `Example` blocks, and
`commands --output json` (the self-describing command tree). This ADR ratifies the direction
that work is already taking, and scopes what remains.

**Correction from the design doc's own verification pass.** The design doc originally
described `internal/api`'s `Mount(routes)` seam as something `/api/v1` could ride directly on
`internal/web`. It cannot: `internal/web/server.go` had no route-extension hook of its own —
that seam exists only on `internal/api`, the separate loopback tofu state backend. The web
mount seam and `internal/service` are being *built* as part of this work, not reused as
pre-existing infrastructure. This ADR ratifies the target, not a claim that the plumbing
already existed.

## Decision

**exocomp exposes its one operational core through three thin programmatic faces — CLI
`--output json`, an HTTP `/api/v1`, and an MCP server — that share one contract and one
execution path. No face is a second way to mutate state.**

1. **One core, thin faces.** Every face calls the same `internal/service` verbs; none reaches
   the store or the queue directly. `internal/service` imports queue/store/reactive/executor/
   secrets and never `net/http`, `cobra`, or `templ`. This is what makes invariant 5 hold for
   machine callers, not just human ones — a bug or a shortcut in one face cannot bypass a
   guardrail another face enforces.

2. **Every write routes through the one queue.** `PlanWorkspace`/`ApplyWorkspace` enqueue via
   `queue.Enqueue`/`EnqueueGated`; nothing behind `/api/v1` or an MCP tool shells out or
   mutates state directly. A destructive call lands in `awaiting_approval` the same way a CLI
   invocation with `--require-approval` does — there is no faster, unguarded path for a
   machine caller than the one a human already has.

3. **Auth reuses ADR-0009 tokens and its three scopes wholesale.** `readonly` / `operator` /
   `approve` map 1:1 onto R / W / A across all three faces. No second session system, no
   second credential type. A non-loopback bind still refuses without an active token; an
   out-of-scope call is rejected before any store touch. MCP tool visibility is scope-gated —
   a `readonly` token advertises only read tools.

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

6. **What has already shipped is the realized foundation, not a promise:** `internal/wire`
   envelope and schema, `--output json` on read/list/status CLI verbs, `doctor --output json`,
   the read-only `/api/v1` endpoints on the ADR-0009 listener, cobra `Example` blocks on every
   command, and `commands --output json`. **What remains proposed and unbuilt:** the
   `internal/service` extraction (verbs are still reached through `RunE`/store/queue directly
   for some paths), the write half of `/api/v1` (plan/apply/run/decide, idempotency-keyed,
   gated), remote CLI client mode (`exocomp login`, `--host`/`EXOCOMP_HOST`), and the MCP
   server. This ADR ratifies the whole direction; it does not claim the whole thing is built.

## Consequences

- Agent-fit becomes a charter concern an operator has explicitly ratified, not an assumption
  a design doc smuggled in — future work on the CLI, `/api/v1`, or MCP can cite this ADR
  instead of re-litigating whether a programmatic surface belongs here at all.
- The three faces are now bound to converge on one marshaller and one set of golden tests; a
  CLI JSON shape and an API JSON shape are the same commitment, not two contracts that can
  drift apart.
- `internal/service` extraction stops being optional cleanup and becomes a prerequisite for
  the write API and MCP faces — both need one verb layer to call without forking logic.
- No new auth story is introduced; ADR-0009's tokens and scopes now cover CLI, HTTP, and MCP
  identically, so a revoked token cuts off all three at once.
- The exit-code table and typed-error taxonomy are now a stable contract: a future change to
  either bumps `schema_version`, the same discipline already ratified for the envelope shape.
- This ADR does not itself accept remote CLI client mode, the write API, or MCP as shipped —
  each remains its own body of work, gated by the usual `make check` and (per invariant 8)
  docs moving with the code that realizes it.

See `docs/design/agent-interface.md` for the full proposal, the phased build plan, and the
evidence trail this ADR ratifies the direction of.
