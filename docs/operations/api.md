# The `/api/v1` operator surface

A JSON API mounted on the same [ADR-0009](../adr/0009-lan-access-tokens.md) token-gated
listener the browser and phone-approval surface use (`internal/web`) — not a second
listener, not a second auth story. Every route reuses the browser's own auth resolution
(session or bearer, the KDF gate, `TouchToken`) and marshals the same `internal/wire`
envelope the CLI's `--output json` emits (see
[Machine-readable output](machine-output.md)), so a resource's API bytes and its CLI bytes
are identical. Design rationale: [agent-interface.md §3.4](../design/agent-interface.md);
recorded in [ADR 0014](../adr/0014-agent-facing-surface.md).

Both the read routes (`internal/web/api.go`) and the write routes
(`internal/web/api_write.go`) are mounted: plan/apply/run/decide are live.

## Auth: bearer + three scopes

Serve the listener with `exocomp serve --web-addr <addr>` or `exocomp web --addr <addr>`
([LAN access](lan-access.md)). Every request needs `Authorization: Bearer <token>` (or the
browser's session cookie); a non-loopback bind refuses to start with no active token.
Tokens carry one of three scopes, checked **before any store read**:

| Scope | May |
|---|---|
| `readonly` | every `GET` route |
| `operator` | every `GET` route, plus every enqueuing `POST` (plan/apply/playbook run) and the maintenance cordon/uncordon |
| `approve` | every `GET` route, plus `POST /approvals/{id}/decision` |

`operator` does **not** cover decisions on this face: a token that can enqueue a gated apply
must not also settle its approval, so `POST /approvals/{id}/decision` takes the `approve`
scope alone, and refuses even that token when the gated job's ledger records it as the
principal that asked (see the [threat model](../security/threat-model.md)). The browser's approve button keeps
its operator-or-approve floor — that is the human at the console, not a credential handed to
an agent. An out-of-scope call gets `403 forbidden` before the request touches the store or
queue.

`GET /approvals/{id}/plan` is the one `GET` route above the `readonly` floor: it opens the
sealed plan, so it takes the same plan-view scope (operator or approve) the browser renders it
behind. It returns a structured, redacted diff — `{destroys, changes, creates, summary,
line_count, artifact_id}`, destroys first — classified through the same core the web plan view
uses, so a secret-backed attribute value the web shows as `[redacted · secret]` is redacted
here too, never carried as plaintext.

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
GET /api/v1/whoami                              # the calling token's own identity + server identity
GET /api/v1/jobs?state=&runbook=&concurrency_key=&limit=
GET /api/v1/jobs/{id}
GET /api/v1/jobs/{id}/events?since=<seq>
GET /api/v1/jobs/{id}/stream                    # SSE, done-on-terminal
GET /api/v1/workspaces
GET /api/v1/sources
GET /api/v1/approvals?all=                      # pending-only unless all=true
GET /api/v1/approvals/{id}                       # + structured stale + provenance (see below)
GET /api/v1/approvals/{id}/plan                 # structured redacted diff (operator/approve, not readonly)
GET /api/v1/approvals/stream                    # SSE, snapshot-on-connect + on-change
GET /api/v1/schedules
GET /api/v1/rules
GET /api/v1/silences?all=                       # active-only unless all=true
GET /api/v1/mutes?all=                           # notify-layer mutes; active-only unless all=true
GET /api/v1/events?source=&outcome=&limit=
GET /api/v1/events/{id}
GET /api/v1/secrets                             # metadata only — no field a value could occupy
GET /api/v1/backups
GET /api/v1/health                              # the same probe `doctor --output json` runs
GET /api/v1/maintenance/readiness               # safe to reboot? (drain)
GET /api/v1/maintenance/interrupted             # jobs a reboot left interrupted, newest first
```

`GET /whoami` reflects the calling token — `{id, name, scope, via, expires_at}` — and adds
`server: {version, hostname}`: the daemon's own build version (`internal/version`) and the
host it runs on, so an identity line reads `exocompd 0.7.4 · pve-01.lan` without probing.
Either server field is omitted when the build carries no version stamp or the host cannot
report its name; the request never fails on a missing hostname.

`jobs`/`events` have no cursor param beyond `since`/`limit`; there is no `?cursor=` yet —
that would be a store change, out of scope for the read routes. `GET /jobs/{id}/stream` reuses
the same append-only `job_event` tail, monotonic `Seq` cursor, and `done`-on-terminal ending
the browser's own job page streams, so the two surfaces cannot diverge.

`GET /approvals/{id}` adds two structured blocks to the approval, both sourced from the
store, never from a rendered sentence, and both carrying identifiers and states only — never
a plan attribute value:

- `stale: {plan_serial, current_serial, moved_by_job, moved_via, moved_at}` — present only
  when the workspace advanced past the saved plan's serial. `moved_by_job`/`moved_via`/
  `moved_at` name the job that first moved it, its trigger, and when, and are omitted when
  the mover is outside the control plane. A fresh approval carries no `stale` block. The
  same block rides the `409 stale_plan` decision error body under `error.stale`.
- `provenance: {origin, alert, rule, matched_at}` — `origin` is `alert` (a reactive rule
  raised it), `manual` (an operator asked by hand), or `schedule` (a schedule fired it). For
  an `alert` origin, `alert`, `rule`, and `matched_at` come from the event ledger row that
  named the job; a `manual`/`schedule` origin omits them.

`GET /approvals/stream` has no per-job terminal state to end on — the pending set itself is
the resource — so it sends one `event: approvals` snapshot on connect (the same
`[]ApprovalView` `GET /approvals` returns, even when empty), then another only when that
projection changes, replacing a phone's poll of `GET /approvals` with one open connection.

## Write endpoints (`operator`)

```
POST /api/v1/workspaces/{name}/plan
POST /api/v1/workspaces/{name}/apply
POST /api/v1/playbooks/run
POST /api/v1/maintenance/cordon                 # drain: stop new fires + claims
POST /api/v1/maintenance/uncordon               # lift the cordon
POST   /api/v1/mutes                            # hold notifications for a scope
DELETE /api/v1/mutes/{id}                       # lift a mute before it expires
```

`POST /mutes` and `DELETE /mutes/{id}` write the notify-layer mute over the same
`store.NotifyMute` the CLI's `mute add`/`rm` and the web banner use — the job still runs;
only its announcement is held (distinct from a silence, which suppresses the rule). The body
is `{scope_kind, scope_value, reason, for | expires_at}`: the scope ladder is `alert`
(this-alert), `host` (one-host), or `global`, and a non-`global` scope needs a `scope_value`.
Exactly one of `for` (a positive Go duration) or `expires_at` (an absolute RFC3339 time in
the future) is required — the store refuses a mute with no future expiry. The create returns
one `mute.get` object `{id, scope_kind, scope_value, reason, created_via, expires_at}`; the
delete returns `204`, and an unknown id is `404`. Both writes are `operator`-scoped (a
bearer, not a browser session); `GET /mutes` is readable by any authed token.

The `maintenance/*` routes drive a planned-reboot drain ([Draining for a planned
reboot](drain-mode.md)): `cordon`/`uncordon` are `operator`-scoped and are **not** enqueues —
a cordon is control-plane state, so there is no `queue.Spec` and no ledger row. All four
return `maintenance.readiness: {cordoned, in_flight, safe_to_reboot}`, except the interrupted
listing, which returns the ordinary `job.list`. A surface that runs no job queue (a
standalone `exocomp web`) answers `503` rather than pretending to cordon a daemon it does not
own.

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
since it was approved settles nothing and returns `409 stale_plan`, not a release. That error
body carries the same structured `error.stale: {plan_serial, current_serial, moved_by_job,
moved_via, moved_at}` block `GET /approvals/{id}` exposes, so a client reads why the plan
cannot apply from fields rather than the message.

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
