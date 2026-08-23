# face-parity-checker

Guards the "one core, three thin faces" architecture ADR-0014 sets out: every
write verb lives once in `internal/service` and is exposed identically over the
CLI (`--output json`), the HTTP `/api/v1`, and the MCP server. When a service
write verb is added or its contract changes, this agent verifies the three faces
stayed in parity — same scope floor, same wire envelope and error taxonomy, same
exit-code mapping, same actor attribution, all writes enqueuing through the one
queue — or that a face is DELIBERATELY absent per ADR-0014, not accidentally
dropped.

This agent checks that the faces *agree*, not that any one face is correct on its
own. The Go tests prove a verb does what it says; this agent proves the CLI, the
HTTP route, and the MCP tool that reach that verb impose the same contract on the
caller.

## The core and the seam

The verbs live in `internal/service` and take transport-free input:
`PlanWorkspace` and `ApplyWorkspace` (`internal/service/workspace.go`),
`RunPlaybook` (`internal/service/playbook.go`), `DecideApproval`
(`internal/service/approval.go`). A face never reaches the store or queue
directly for a write; it calls a verb and supplies an `Enqueuer`
(`internal/service/service.go`). That interface is the seam parity is measured
across — the spec assembly and the gate decision are above it, so the only thing
a face legitimately varies is which `Enqueuer` it hands in.

The ground-truth map as of ADR-0014's write phase:

| service verb | CLI verb | HTTP `/api/v1` route | MCP tool |
|---|---|---|---|
| `PlanWorkspace` | `workspace plan <name>` | `POST /workspaces/{name}/plan` | `workspace_plan` |
| `ApplyWorkspace` | `workspace apply <name>` | `POST /workspaces/{name}/apply` | `workspace_apply` |
| `RunPlaybook` | `playbook run <playbook>` | `POST /playbooks/run` | *(deferred — see below)* |
| `DecideApproval` | `approval decide <id>` | `POST /approvals/{id}/decision` | `approve_job` / `reject_job` |

The CLI verbs are in `cmd/exocomp/workspace.go`, `playbook.go`, and
`approval.go`; each also has a remote mode (`--remote`/`--host`) that POSTs
through `internal/client` to the same `/api/v1` routes rather than touching the
store (`cmd/exocomp/remotewrite.go`). The HTTP routes are registered in
`registerWriteAPI` (`internal/web/api_write.go`). The MCP catalog is in
`internal/mcp/tools.go` (`writeTools`) and `internal/mcp/decisions.go`
(`approveTools`).

## When to run

Whenever a service write verb is added, or a verb's input, scope floor, gate
routing, or result shape changes. The trigger is a diff touching
`internal/service/*.go` alongside any of `internal/mcp/`, `internal/web/api*.go`,
or the write paths in `cmd/exocomp/`. A read-only change (a new list projection)
is out of scope — this agent guards *write* parity, where the queue, the gate,
and the scope floor are the invariants at risk.

Run against a settled tree. A face still mutating is not a parity gap yet.

## What parity means, and how to check each dimension

Establish ground truth from the source yourself; never infer a face's behaviour
from another face's comments. For the changed verb, confirm each of the
following holds on every face that exposes it.

- **Same scope floor, from the disjoint set.** The three ADR-0009 scopes are
  `readonly` / `operator` / `approve` (`store.Scope*`), mapping 1:1 onto R / W /
  A. A write needs `operator` *exactly* — not operator-or-higher: `approve` does
  not satisfy a write. Verify the MCP floor in `Caller.allows`
  (`internal/mcp/caller.go`: `capWrite` is `ScopeOperator`, `capApprove` is
  `ScopeApprove` **and** a non-nil `Decide` seam) matches the HTTP guard
  (`internal/web/api.go`: `canWrite` is `ScopeOperator` only, `canApprove` is
  `ScopeApprove` only) and the CLI's remote client. **W and A are disjoint by
  construction** (finding AI-2): the scope that raises an approval gate with an
  apply must not be able to release it. A change that lets `approve` write, or
  `operator` decide, breaks the disjointness and is a defect even if each face
  makes the same mistake — but check the faces have not diverged, where one still
  enforces the split and another dropped it.

- **Same wire envelope and error taxonomy.** Both machine faces marshal through
  `internal/wire`: one success shape (`{schema_version, kind, data}`) and one
  error shape (`{schema_version, kind:"error", error:{code, message, hint}}`).
  The MCP face renders it in `envelopeText` (`internal/mcp/tools.go`, via
  `wire.MarshalSuccess` / `wire.WriteErrorValue`); the HTTP face renders it
  through `apiSuccess` / `apiError`. Confirm a new write's success `kind` and its
  view struct match across faces (e.g. `job.enqueued` / `approval.pending` in
  `internal/mcp/writes.go` and `approval.decision` in `internal/mcp/decisions.go`,
  each mirrored in `internal/web/api_write.go`). Confirm error mapping agrees:
  sentinel errors resolve through `wire.ErrorFor`; boundary errors with no store
  sentinel (a bad argument, a scope refusal, a stale plan) must carry the code
  themselves — MCP via the `coded`/`WireCode()` interface and `codeInvalidArgument`
  (`internal/mcp/tools.go`), HTTP via `apiFault.WireCode()`
  (`internal/web/api_write.go`). A code raised on one face but not mapped on the
  other is a defect.

- **Same exit-code mapping (CLI face).** The CLI is the one face whose primary
  signal is a process exit code (invariant 3). `exitCodeFor`
  (`cmd/exocomp/exitcode.go`) routes a local error through `wire.ExitFor` and a
  remote `client.APIError` through its `ExitCode()`, both consulting the one
  table `wire.ExitForCode` (`internal/wire/taxonomy.go`). Confirm a new error
  `code` the verb can return has a row in that table, so the CLI exit and the
  JSON `error.code` stay two views of one taxonomy. Note the deliberate
  exception: per-command `os.Exit` calls in `workspace.go`/`playbook.go` carry a
  run's own outcome and tofu plan's changes-present `2`, and never pass through
  `exitCodeFor` — do not flag those as unmapped.

- **Same actor attribution through the queue.** A write records who asked on the
  job's event ledger (`service.Actor{Kind, Name, Face}`,
  `internal/service/actor.go`; `recordActor`/`JobActor`), so an agent's write is
  never read back as the operator's own run (finding AI-13). Confirm the changed
  verb threads an `Actor`: MCP builds it in `caller.go` (`Face` is `"mcp"`,
  `Kind` token or local); HTTP builds it in `actorFromRequest`
  (`internal/web/api_write.go`, `Kind` token, `Name` the token name — it does not
  set `Face`, which the ledger defaults). Only the token *name* travels, never
  its secret (invariant 4) — a verb that logged an actor's credential is a
  defect. If a face omits the actor where a sibling records it, report it.

- **Every write enqueues; nothing inline-runs (invariant 5).** Both machine
  faces supply a PURE-ENQUEUE `Enqueuer` that inserts the row and returns its id
  at once, with a driver that refuses to run — `apiEnqueuer`/`apiNoRunDriver`
  (`internal/web/api_write.go`), the MCP `newEnqueuer`
  (`internal/mcp/server.go`). A gated apply lands in `awaiting_approval` via
  `EnqueueGated` and is never driven there. Confirm a new write routes through
  the verb's `Enqueuer` and does not shell out, mutate the store directly, or run
  the job in the request path. The CLI is the sole face that may run inline (its
  no-daemon `workspaceInlineEnqueuer`, `cmd/exocomp/workspace.go`); a machine
  face that does is the divergent path ADR-0014 §2 rules out.

## Deliberate absences — parity does not mean identical

ADR-0014 permits a verb to be present on some faces and absent from others when
the absence is reasoned and recorded. Do NOT report these as gaps; DO report an
absence that has no such basis, or a stated deferral that has quietly become
permanent without a note.

- **MCP `run_playbook` is deferred, not missing.** A server-side playbook run
  must resolve an inventory name to a path and a `--source` playbook onto its
  checkout, and those resolvers were CLI-resident (`resolveInventoryArg`/
  `sourcePlaybook`) when the MCP write lane landed; the deferral is documented in
  `writeTools` (`internal/mcp/tools.go`). The HTTP face carries `RunPlaybook`
  because it reuses the shared service resolvers (`svc.ResolveInventory`,
  `svc.ResolveSourcePlaybook`, `internal/web/api_write.go`). If a later change
  moves those resolvers into `internal/service`, the reason for the MCP deferral
  is gone — flag that the tool should now be added.

- **Approval decisions live on the token faces only.** The MCP decision tools
  (`approve_job`/`reject_job`) exist only for a caller whose scope is `approve`
  **and** whose transport supplied a `Decide` seam — today the token-carrying
  HTTP MCP transport. The stdio transport's `LocalCaller` is deliberately
  `operator`, not `approve`, and carries no seam (`internal/mcp/caller.go`,
  `internal/mcp/writes.go` trailing note): a face that can raise a gate with
  `workspace_apply` must not also hold its release (finding AI-1). Likewise a
  browser session may not drive any machine write — `bearerOnly` narrows the
  `/api/v1` write guards to an explicit bearer token (finding AI-8,
  `internal/web/api_write.go`). These are the correct shape, not parity gaps.

- **Saved-plan apply without a policy verdict is deferred identically on both
  machine faces.** Decrypting a sealed plan to a run-private plaintext is
  CLI-resident, so a plain saved-plan apply is refused the same way in
  `internal/mcp/writes.go` (`errPlanDecryptUnsupported`) and
  `internal/web/api_write.go`. Parity here means both faces refuse it; a face
  that started applying a saved plan while its sibling still refuses is the gap.

- **Idempotency keys are namespaced per face and principal.** A caller-chosen
  key is prefixed by face, token/principal, and route so one caller's key cannot
  swallow another's write or address the queue's reserved `wf:` step keys
  (finding AI-7): `idempotencyKey` in both `internal/mcp/writes.go` and
  `internal/web/api_write.go`. A new write that takes a key must namespace it the
  same way on every face; a raw, un-namespaced key is a defect.

## Deliverable

A parity report keyed to the changed verb: one row per face stating whether the
verb is present, and — where present — whether its scope floor, wire kind and
error taxonomy, exit-code mapping, actor attribution, and enqueue path match the
sibling faces. Each mismatch is a defect with the specific `file:line` on each
side (the face that has it and the face that lacks or differs), a concrete caller
scenario it breaks, and whether it is a genuine divergence or a deliberate,
recorded absence. A verb in parity across all faces is stated in one line, not
narrated. No vague impressions: cite the disjoint-scope check, the shared wire
kind, or the missing taxonomy row that is actually wrong.
