# The MCP surface

One MCP server, one transport. `POST /mcp` speaks it over HTTP on the same
[ADR-0009](../adr/0009-lan-access-tokens.md) token-gated listener the browser surface and
[`/api/v1`](api.md) already use — not a second listener, not a second auth story. Design
rationale: [agent-interface.md §3.5](../design/agent-interface.md); recorded in
[ADR 0014](../adr/0014-agent-facing-surface.md).

There is no bespoke stdio MCP server, and none is coming back: a process running at the
local operator's ambient authority with no credential and no scope is exactly the shape
the [threat model](../security/threat-model.md) ruled out. A stdio-only MCP
client bridges to `POST /mcp` with an off-the-shelf http-to-stdio wrapper — see
[Bridging a stdio client](#bridging-a-stdio-client) below. An agent working the local
filesystem alongside exocomp is the sanctioned LOCAL path and drives the
[agent-aware CLI](machine-output.md) directly (`--output json`, exit codes) — it needs no
MCP hop at all.

Every tool call and resource read routes to the same `internal/service` verbs the CLI and
the HTTP API call, and marshals the same `internal/wire` envelope, so a resource's MCP
bytes match its `--output json` and its `/api/v1` bytes. Writes enqueue through the one job
queue and return at once; a running `exocomp serve` claims and runs the row.

## Connecting

Serve the listener with `exocomp serve --web-addr <addr>` or `exocomp web --addr <addr>`
([LAN access](lan-access.md)). The MCP endpoint comes up with it — there is no separate
flag and no separate port. Mint a token at the scope the agent actually needs:

```sh
exocomp token new tailnet-agent --scope readonly    # or operator, or approve
```

Then point the client at the endpoint with that token as a bearer:

```
endpoint:  https://<host>:<port>/mcp
header:    Authorization: Bearer <token>
```

The handshake and a first call, by hand:

```sh
curl -sS https://<host>:<port>/mcp \
  -H "Authorization: Bearer $EXOCOMP_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'

curl -sS https://<host>:<port>/mcp \
  -H "Authorization: Bearer $EXOCOMP_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

The listener serves a self-signed certificate by default; pin its fingerprint the way
[`exocomp login`](remote-access.md) does, or front it with a Tailscale/Caddy endpoint that
terminates TLS.

## Scopes decide the surface

The three ADR-0009 scopes map 1:1 onto read / write / approve, and **`tools/list` is
filtered by the caller's scope** — an agent is never shown a verb its credential cannot
exercise. The same floor is re-checked on every `tools/call`, before any store read, so a
call to a tool outside the caller's scope is refused rather than executed.

| Scope | `tools/list` advertises | Notably absent |
|---|---|---|
| `readonly` | `list_jobs`, `get_job`, `get_job_events`, `list_workspaces`, `get_workspace`, `get_drift`, `list_approvals`, `list_sources`, `list_schedules`, `list_rules`, `list_events`, `explain_event`, `list_secrets` (metadata), `list_backups`, `health` | every write, every decision |
| `operator` | the read tools, plus `workspace_plan` and `workspace_apply` | every decision |
| `approve` | the read tools, plus `approve_job` and `reject_job` | every write |

**`operator` and `approve` are disjoint on purpose.** A token that can raise the approval
gate with `workspace_apply` must not also hold its release, and a token that can release a
gate must not be able to raise one. On top of that, a decision is refused when the gated
job's ledger records the deciding token as the principal that asked for it. Both rules live
in the one decision path `/api/v1` uses, so neither face can drift from the other. See
[the threat model](../security/threat-model.md).

Resources — `exocomp://workspaces`, `exocomp://jobs/{id}`, `exocomp://jobs/{id}/log`,
`exocomp://workspaces/{name}/drift`, `exocomp://approvals/pending`, `exocomp://schedules`,
`exocomp://events/recent`, `exocomp://health` — are reads, available to every scope.

`run_playbook` is deliberately not exposed on the MCP surface yet: the inventory and
`--source` path resolvers it needs are still CLI-resident. Use
[`POST /api/v1/playbooks/run`](api.md) for that.

### Bridging a stdio client

A harness that only speaks the MCP 2024-11-05 stdio shape (no HTTP transport of its own)
bridges to `POST /mcp` with an off-the-shelf stdio-to-HTTP wrapper — exocomp does not ship
or endorse one; any spec-conformant wrapper works. Two examples, run as a subprocess of the
harness:

```sh
npx mcp-remote https://<host>:<port>/mcp --header "Authorization: Bearer $EXOCOMP_TOKEN"
```

```sh
npx supergateway --stdio "https://<host>:<port>/mcp" --header "Authorization: Bearer $EXOCOMP_TOKEN"
```

The wrapper is the transport; it carries no authority of its own beyond the bearer token you
give it, so mint the token at the scope the agent actually needs (see below) rather than
handing it an `operator` or `approve` token by default.

## What the transport does, and does not, implement

- **One endpoint.** `POST /mcp` carries one client→server JSON-RPC message, or a batch of
  them as a JSON array. The responses come back in that POST's body as
  `application/json` — an array if the request was an array.
- **Notifications get `202 Accepted` and an empty body.** There is no id to answer on. A
  side-effecting method (`tools/call`, `resources/read`) sent *as* a notification is
  refused with `invalid_request` instead of being executed silently, so a mutation always
  leaves a frame for an audit wrapper to record.
- **`GET /mcp` is `405`.** Streamable-HTTP allows a client to hold a GET open for
  server→client messages; this server initiates none. Job progress already has its own SSE
  tail at `GET /api/v1/jobs/{id}/stream`, which re-checks the token as it polls and ends
  the stream when it is revoked.
- **No sessions, and no `Mcp-Session-Id`.** This is a security property, not a gap: every
  message re-runs the bearer resolution and re-checks the scope, so a **revoked or
  downgraded token stops working on its very next call** rather than at the end of a
  long-lived session. Re-sending `initialize` costs nothing.
- **Protocol version `2024-11-05`**, echoed by `initialize`.
- **`initialize` returns an `instructions` string.** It carries one thing an agent
  cannot get from tool names alone: "event" spans two unrelated id-spaces —
  `get_job_events` reads a job's own execution log; `list_events`/`explain_event`
  read inbound reactive trigger events. Do not conflate the two.

## Errors come at three levels

| Level | Looks like | Examples |
|---|---|---|
| Transport | an HTTP status + a `wire` error envelope | `401` no/invalid bearer, `403` a browser session or an unknown scope, `429` auth backoff (`Retry-After`), `503` KDF gate saturated |
| Protocol | HTTP `200` + a JSON-RPC `error` object | `-32700` unparseable body, `-32600` invalid request, `-32601` unknown method, `-32602` unknown tool |
| Tool | HTTP `200` + a result with `isError: true`, holding a `wire` error envelope | `forbidden` (scope), `invalid_argument`, `not_found`, `conflict`, `stale_plan` |

An unauthenticated request is **never** answered with a JSON-RPC frame: it gets the `401`,
so an agent cannot mistake a refusal for a session.

## Idempotency, attribution, and what never travels

`workspace_plan` and `workspace_apply` take an optional `idempotency_key` argument, so a
retried call dedups to one job. The key is namespaced by face, authenticated token, and
tool before it reaches the queue — one agent's guessable key cannot collide with another
caller's write.

Every MCP write records the token behind it on the job's event ledger (`enqueued by token
<name> via mcp`), so an agent's action never reads back as the operator's own CLI run.

Values never travel, by construction: `list_secrets` returns metadata only, `get_drift`
carries resource addresses, actions, and attribute *keys* but no values, and no tool returns
a secret plaintext or an **approval decision link** — a decision link is a single-use HMAC
capability, and returning one to an agent would silently promote it to approver. A decision
returns its outcome; the capability is derived and consumed server-side.

## Later: OAuth

The MCP specification's own authorization story is OAuth 2.1, and some hosted clients
expect it. exocomp deliberately does not implement it yet: ADR 0014 §3 commits to ADR-0009
bearer tokens and their three scopes across all three faces, and one credential type is the
whole point. The seam is narrow when it is wanted — `apiGuard`/`resolve` is the single place
a request becomes an `identity`, and `mcpCaller` is the single place an identity becomes an
MCP principal — so an OAuth resolver would slot in there and leave the catalog, the scope
floors, and every handler untouched.

Related: [The API surface](api.md) · [Machine-readable output](machine-output.md) ·
[LAN access](lan-access.md).
