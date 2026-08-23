# The `/api/v1` operator surface

A JSON API mounted on the same [ADR-0009](../adr/0009-lan-access-tokens.md) token-gated
listener the browser and phone-approval surface use (`internal/web`) — not a second
listener, not a second auth story. Every route reuses the browser's own auth resolution
(session or bearer, the KDF gate, `TouchToken`) and marshals the same `internal/wire`
envelope the CLI's `--output json` emits (see
[Machine-readable output](machine-output.md)), so a resource's API bytes and its CLI bytes
are identical. Design rationale: [agent-interface.md §3.4](../design/agent-interface.md);
ratified in [ADR 0014](../adr/0014-agent-facing-surface.md).

Both the read routes (`internal/web/api.go`) and the write routes
(`internal/web/api_write.go`) are mounted today — plan/apply/run/decide are live, not a
future wave.

## Auth: bearer + three scopes

Serve the listener with `exocomp serve --web-addr <addr>` or `exocomp web --addr <addr>`
([LAN access](lan-access.md)). Every request needs `Authorization: Bearer <token>` (or the
browser's session cookie); a non-loopback bind refuses to start with no active token.
Tokens carry one of three scopes, checked **before any store read**:

| Scope | May |
|---|---|
| `readonly` | every `GET` route |
| `operator` | every `GET` route, plus every enqueuing `POST` (plan/apply/playbook run) |
| `approve` | every `GET` route, plus `POST /approvals/{id}/decision` |

`operator` also covers decisions (the same floor the browser's approve button enforces).
An out-of-scope call gets `403 forbidden` before the request touches the store or queue.

## The envelope

Every response — success or failure — is one shape:

```json
{ "schema_version": 1, "kind": "job.list", "data": [ … ] }
{ "schema_version": 1, "kind": "error",
  "error": { "code": "concurrency_held", "message": "…", "hint": "wait for job 412 or cancel it" } }
```

`schema_version` is a pinned contract; a shape change bumps it. `data` is never omitted on
success — an empty list still renders `"data":[]`, so an agent can tell "no rows" from
"wrong kind." Field values are values-free by construction: identifiers, states, keys, and
counts travel; attribute values, ciphertext, and token hashes never do (the `wire` package's
projection types have no field they could occupy).

## Read endpoints (`readonly`+)

```
GET /api/v1/jobs?state=&runbook=&concurrency_key=&limit=
GET /api/v1/jobs/{id}
GET /api/v1/jobs/{id}/events?since=<seq>
GET /api/v1/jobs/{id}/stream                    # SSE, done-on-terminal
GET /api/v1/workspaces
GET /api/v1/sources
GET /api/v1/approvals?all=                      # pending-only unless all=true
GET /api/v1/approvals/{id}
GET /api/v1/schedules
GET /api/v1/rules
GET /api/v1/silences?all=                       # active-only unless all=true
GET /api/v1/events?source=&outcome=&limit=
GET /api/v1/events/{id}
GET /api/v1/secrets                             # metadata only — no field a value could occupy
GET /api/v1/backups
GET /api/v1/health                              # the same probe `doctor --output json` runs
```

`jobs`/`events` have no cursor param beyond `since`/`limit`; there is no `?cursor=` yet —
that would be a store change, out of the read lane's scope. `GET /jobs/{id}/stream` reuses
the same append-only `job_event` tail, monotonic `Seq` cursor, and `done`-on-terminal ending
the browser's own job page streams, so the two surfaces cannot diverge.

## Write endpoints (`operator`)

```
POST /api/v1/workspaces/{name}/plan
POST /api/v1/workspaces/{name}/apply
POST /api/v1/playbooks/run
```

Every write is a **pure enqueue**: the route builds a `queue.Spec` through the same
`internal/service` verb the CLI calls, inserts the row, and returns at once — it never runs
the job itself. A running `exocomp serve` on the same data dir claims and runs the row on
its own poll.

- **`POST /workspaces/{name}/plan`** — body `{run_secrets, timeout, idempotency_key via header}`.
  Returns `job.enqueued: {job_id, state}`. The plan runs under the "api" trigger so the
  daemon seals its `-out` file into a consumable artifact post-run (a CLI plan seals its
  own); a later apply names that artifact by id.
- **`POST /workspaces/{name}/apply`** — body `{run_secrets, timeout, plan: "<artifact id>"}`.
  No `plan` is a fresh, auto-approved apply. With a saved plan, its recorded policy verdict
  decides the outcome: a `deny` verdict is refused (`409 conflict`), a `require_approval`
  verdict lands the job in `awaiting_approval` and returns `approval.pending: {approval_id,
  job_id, state, expires_at, ttl_seconds, policy, plan_artifact_id}`, and a clean verdict
  applies straight through. The apply enqueues against the **sealed** plan path; the daemon
  that claims the row decrypts it into a run-private plaintext for that one run — this
  pure-enqueue face never leaves a plaintext plan in the shared data dir.
- **`POST /playbooks/run`** — body `{playbook, inventory, vault_project, run_secrets,
  source, limit, tags, skip_tags, check, diff, extra_vars, tolerate_unreachable, timeout}`.
  `inventory` and `source` names resolve through the same service resolvers the CLI uses. A
  `source` whose checkout declares `.exocomp/hooks.yml` is refused with `501` — that
  lifecycle is a composed workflow the CLI owns, not one queued play, and is not offered on
  this face yet.

## Deciding an approval

```
POST /api/v1/approvals/{id}/decision   {"decision": "approve" | "reject"}   (scope operator or approve)
```

Routes through the same fail-closed `reactive.Decide` a decision link and `approval decide`
use. The capability token is **derived server-side** from the envelope, never carried by the
caller — a scope is not a bearer capability link. A workspace that moved under a saved plan
since it was approved settles nothing and returns `409 stale_plan`, not a release.

## Idempotency

Every mutating request accepts `Idempotency-Key: <caller-chosen string>`. When a job already
holds that key, `Enqueue` returns the **existing** job unchanged instead of creating a
second one — a retried request from a flaky connection or a retrying agent cannot
double-apply.

## Errors: code, HTTP status, and the CLI exit code

Every failure maps one taxonomy `code` onto one HTTP status:

| `code` | HTTP status | Source |
|---|---|---|
| `not_found` | 404 | `store.ErrNotFound` |
| `conflict` / `concurrency_held` / `stale_plan` / `duplicate` | 409 | store/reactive sentinels |
| `invalid_argument` | 400 | a malformed id, filter value, or JSON body |
| `unauthenticated` | 401 | no valid token/session (`WWW-Authenticate: Bearer` set) |
| `forbidden` | 403 | token scope too low for the route |
| `kdf_busy` | 503 | the auth gate is saturated (`Retry-After: 1` set); retry |
| anything unmatched | 500 | `internal` |

See [Machine-readable output](machine-output.md) for the shared `code` taxonomy, the
process exit-code table it maps onto (`client.APIError.ExitCode()`), and how much of that
mapping actually reaches the CLI's own exit status today.

Related: [Remote CLI](remote-access.md) · [LAN access](lan-access.md) ·
[Reacting to alerts](reactive.md) · [Gating tofu plans](plan-policy.md).
