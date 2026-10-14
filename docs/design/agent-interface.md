---
type: Design
title: "Agent interface"
description: "The three programmatic faces of remote CLI, HTTP API and MCP, the wire envelope, and the error and exit-code taxonomy."
status: stable
tags: [design, api, mcp]
---

# Agent interface — one core, three programmatic faces

Status: accepted — the decision is recorded in Decision 0014 (agent-facing surface)

## 1. Vision

`exocompd` is already designed as **one operational core with thin faces** — the
architecture overview states it plainly: `internal/service` is meant to own *all*
business logic ("no HTTP, no terminal, no HTML — so no client can have a divergent code
path"), `internal/api` is a thin HTTP/JSON layer over it, and TUI / web / CLI / webhooks
are faces (docs/architecture/overview.md:93-104). Today only the human faces exist. This
document adds the **programmatic faces a machine operator needs** — a remote CLI client, an
operator HTTP/JSON API, and an MCP server — as three thin reflections of the *same* core,
never as parallel execution paths.

The primary goal is concrete: **run homelab-ansible ops from an operator workstation, over
the tailnet, against a remote host's exocomp API** — plan/apply a workspace,
run a playbook, watch a job, decide an approval — without SSHing into that host and without
scraping aligned text. The same surface must be **local-AI-friendly**: a weak, small-context
model should be able to drive it by branching on integers and typed codes, not by parsing
prose.

Four design commitments make that possible, and every one of them already exists as a core
primitive we are exposing rather than inventing:

- **Exit-code-is-truth.** Subprocess correctness is decided by exit codes only; parsed
  output only enriches the display (AGENTS.md invariant 3; internal/queue/queue.go:357-379).
  A machine branches on a number.
- **Values-free, high-signal structured output.** The drift payload already emits resource
  address + actions + attribute *keys* and never values, because "a value is often a secret"
  (internal/executor/tofu_drift.go:9-31). That is the template for the whole contract:
  identifiers, states, keys, counts — never values, ciphertext, or full stdout.
- **Deterministic, cache-aware reads.** Append-only `job_event` is tailed by a monotonic
  `Seq` cursor and ends on a `done` event (internal/web/stream.go:49-78); the workspace
  bearer is HKDF-deterministic (internal/api/server.go:32-37). Stable bytes for unchanged
  state make repeated agent polls diffable and prompt-cache-stable.
- **One queue, no side doors.** Every execution path converges on the job queue before
  anything runs; guardrails live in the queue, not in configuration (AGENTS.md invariant 5;
  docs/architecture/overview.md:48-72). Every new face **enqueues** — it never shells out.

## 2. Current-surface reality and the gap

### 2.1 What consumes exocomp today

| Surface | Transport | Shape | Auth | Machine-usable? |
|---|---|---|---|---|
| `exocomp` CLI | local process, reads data dir, `Drain`s its own jobs | aligned human text (272 `Fprintf`/tabwriter sites; zero `--json`/`--output` flag; zero cobra `Example`) | filesystem perms on data dir | No — screen-scrape only |
| TUI | local + ssh (Wish) | Bubble Tea render | data-dir / ssh | No |
| Web + phone approval | `--web-addr` TLS listener | templ **HTML** behind session cookie | Decision 0009 bearer→session, three scopes | No — HTML only |
| OpenTofu state backend | loopback `127.0.0.1` only | JSON | HKDF-derived per-workspace bearer | Yes, but **workspace-state only**, not general ops (internal/api/state.go) |
| `/metrics`, `/healthz` | daemon listener | Prometheus / text | none | Read-only telemetry |

The **only** machine-readable HTTP is the loopback tofu state backend and metrics. The
`internal/service` layer that is supposed to hold business logic is a three-line stub
(internal/service/doc.go); the logic lives inside cobra `RunE` bodies, the store, the queue,
and reactive. `internal/api` is *only* the tofu backend, bound loopback-only.

### 2.2 The gap to the primary goal

To run ops from `workstation` over the tailnet to `server`, four things are missing:

1. **No general operator API.** The tofu backend speaks only tfstate/tflock and binds
   loopback; there is no `/api/v1/jobs`, `/workspaces`, `/approvals`. The web listener carries
   the right auth (Decision 0009) but emits HTML.
2. **No remote CLI client mode.** The CLI always operates locally against the data dir. There
   is no `--remote` / `EXOCOMP_REMOTE` that makes it a thin HTTP client of a remote daemon.
3. **No structured-output contract.** Nothing emits a stable, versioned, values-free JSON an
   agent or small model can branch on. Errors are flat `{"error": msg}` (internal/api/state.go)
   or free-text prose; there is no typed `{code, message, hint}`.
4. **No MCP face.** No tool/resource catalog, so an agent harness cannot discover or drive
   exocomp as tools.

The tailnet itself is already the intended remote path (docs/architecture/platform-contract.md:47-49
"wizard detects a tailnet and configures the TCP listener bound to the tailnet address"), and
Decision 0009 already delivers the credential (`token new`), the transport guard (non-loopback bind
refuses without a token), and the pinning material (self-signed cert fingerprint printed for
trust-on-first-use). The pieces are on the bench; they are not assembled into a programmatic
operator surface.

## 3. Target architecture

```
        workstation (operator workstation / agent host)
        ┌───────────────┬──────────────┬───────────────┐
        │ exocomp CLI   │ HTTP client  │ MCP client     │
        │ (local/--remote)│ (curl/SDK) │ (bearer /mcp)  │
        └───────┬───────┴──────┬───────┴───────┬────────┘
                │  tailnet (bearer token + pinned cert / Tailscale Serve TLS)
   server ─────┼──────────────┼───────────────┼──────────────────────
        ┌───────▼──────────────▼───────────────▼────────┐
        │  Decision 0009 token-gated listener (internal/web)  │  ← one auth story
        │  /            (HTML, existing)                 │
        │  /api/v1/...  (JSON, new)  ── requireAuth+scope │
        │  POST /mcp    (MCP over HTTP, bearer+scope)    │
        └───────────────────────┬────────────────────────┘
                                ▼
                    internal/service  ← ALL business logic (verbs)
                                ▼
             queue.Enqueue / EnqueueGated / Release  ← the one execution path
                                ▼
                    store · reactive · executor · secrets
```

### 3.1 One operational core — extract `internal/service`

Every programmatic face calls the **same** service verbs; none reaches the store or the queue
directly. This is the load-bearing decision: it is what makes invariant 5 hold for agents, and
it is what lets the CLI, the API, and MCP share one marshaller and one set of golden tests.

Proposed verbs (thin wrappers over today's `RunE` logic):

```
ListJobs / GetJob / GetJobEvents / FollowJob
ListWorkspaces / GetWorkspace / GetWorkspaceDrift
PlanWorkspace / ApplyWorkspace              (enqueue / EnqueueGated)
RunPlaybook / RunCommand                    (enqueue, --require-approval gates)
SyncSource / ListSources
ListApprovals / DecideApproval              (reactive.Decide, fail-closed)
ListSchedules / ListRules / ListSilences / ListEvents / ExplainEvent
ListSecretsMeta                             (metadata only — no plaintext code path)
ListBackups / Health                        (doctor summary)
```

The service package imports queue, store, reactive, executor, secrets — never `net/http`,
`cobra`, or `templ`. Sink-time redaction (internal/queue/redact.go) and gate defaults are
inherited for free by everything above it.

### 3.2 The structured-output contract (`internal/wire`)

A single presentation-only package holds the envelope, the schemas, and the error taxonomy,
and is the **only** marshaller the CLI `--output json`, the HTTP API, and MCP tool results use.
One shared marshaller under golden tests is what keeps the three faces byte-identical.

**Envelope.** Every success and every failure is one shape:

```json
{ "schema_version": 1, "kind": "job.list", "data": [ … ] }
{ "schema_version": 1, "kind": "error",
  "error": { "code": "concurrency_held", "message": "…", "hint": "wait for job 412 or cancel it" } }
```

- `schema_version` is a pinned contract; a shape change bumps it (prompt caches and parsers
  depend on it).
- `kind` names the payload so an agent can dispatch without guessing.

**Values-free and low-token**, following the drift precedent (tofu_drift.go): emit identifiers,
states, keys, counts — never attribute values, ciphertext, or full stdout. A job is
`{id, runbook, kind, trigger, state, exit_code, partial, queued_at, started_at, finished_at}`;
the log body is fetched separately, cursor-paged. `omitempty` everywhere, so a quiet system
returns a tiny document.

**Deterministic ordering.** Sort by a stable key exactly as the store already orders
(`ListJobs` oldest-first, internal/store/job.go:216; tokens by id). Never map-iteration order.
Timestamps are fixed RFC3339 UTC (the store normalizes to UTC ms). Volatile fields (a relative
"age", a "now") are omitted from the cacheable body — let the agent compute them.

**Typed error taxonomy**, mapped 1:1 onto the store/reactive sentinels so an agent branches on
`code`, never on a sentence:

| `code` | Source sentinel | Meaning |
|---|---|---|
| `not_found` | store.ErrNotFound (store.go:20) | no such entity |
| `conflict` | store.ErrStateConflict (store.go:26) | wrong state for the transition |
| `duplicate` | store.ErrDuplicate (store.go:28) | name/key already taken |
| `concurrency_held` | store.ErrConcurrencyHeld (store.go:31) | another run holds the workspace |
| `stale_plan` | reactive.ErrStalePlan (approval.go:27) | workspace moved under the saved plan |
| `kdf_busy` | web.ErrKDFBusy (auth.go:41) | auth gate saturated; retry (transient 503) |
| `streams_over_cap` | the SSE stream cap (stream.go) | too many live streams; close a tab or retry |
| `unauthenticated` | web.ErrUnauthenticated | no valid token/session |
| `forbidden` | scope check (auth.go:241) | token scope too low for this verb |

**Exit-code table** (the primary agent signal — codify what invariant 3 already gives). Kept
deliberately small, and defined in the same record as the contract:

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | generic failure (a run settled failed; a request errored) |
| 2 | changes present — `tofu plan -detailed-exitcode` (cmd/exocomp/workspace.go:432-436) |
| 3 | not found |
| 4 | unauthenticated / forbidden |
| 5 | conflict / concurrency held / locked |
| 6 | awaiting approval (a gated write landed pending) |
| 7 | busy — retry later (kdf_busy) |

The JSON `error.code` and the process exit code are two views of the same taxonomy; a weak
model can use either.

### 3.3 Remote CLI client mode

The CLI gains a **client mode**: when a host is configured it becomes a thin HTTP client of a
remote daemon instead of operating on a local data dir. Same verbs, same `--output json`, same
envelope — the difference is transport.

```
exocomp login server.tailnet.ts.net        # TOFU: fetches cert, shows fingerprint,
                                             # stores pinned fp + token (known-hosts style)
exocomp --remote server jobs list --output json
EXOCOMP_REMOTE=server EXOCOMP_TOKEN=… EXOCOMP_OUTPUT=json exocomp workspace plan web
exocomp --remote server approval decide 42 --approve
```

- Config precedence: `--remote` flag → `EXOCOMP_REMOTE` → a valueless `--remote` fills from the
  `~/.config/exocomp/hosts.toml` default. Token from `--token` → `EXOCOMP_TOKEN` → the host
  entry. A bare `exocomp <cmd>` with neither flag nor env stays local against the data dir
  (unchanged), so nothing an operator on `server` does today breaks. The selector is `--remote`,
  not `--host`: `--host` is already the ansible target pattern (`exocomp command --host …`), so
  reusing it for the daemon would collide.
- `internal/client` is the typed HTTP client (the `api/` "monorepo seam" package the layout
  reserves) generated from / kept in lockstep with the API's OpenAPI 3.1 spec.
- New global flags land here and apply in both modes: `--output text|json`, `--remote`,
  `--token`, `--idempotency-key`, and the non-interactive contract (`--assume-yes`,
  `--no-input`, replacing the divergent `--force` / `--yes-really`).

### 3.4 Operator HTTP/JSON API (`/api/v1`, mounted on the Decision 0009 listener)

The general API is **not** a new listener and **not** a second auth story. It is JSON routes
added to the existing token-gated web mux (internal/web/server.go:121-148), behind the same
`requireAuth` + scope check, wrapped by the same `securityHeaders`. The web server already reads
the store directly "so there is one authentication story (the LAN token), not two"
(server.go:58-60) — the JSON API rides that exact seam. `internal/web` has no route-extension
hook today (the `Mount` seam lives only on the separate loopback backend `internal/api`), so
the first step is a small mount seam on `internal/web` mirroring it; after that the JSON routes
sit behind the same `requireAuth`+scope. The loopback tofu backend stays as it is; this is
orthogonal.

Read endpoints (scope `readonly`+):

```
GET /api/v1/jobs?state=&runbook=&concurrency_key=&limit=&cursor=   → mirrors store.JobFilter
GET /api/v1/jobs/{id}
GET /api/v1/jobs/{id}/events?since=<seq>                            → cursor-paged, redacted
GET /api/v1/jobs/{id}/stream                                        → SSE, reuses stream.go
GET /api/v1/workspaces  ·  /workspaces/{name}  ·  /workspaces/{name}/drift
GET /api/v1/approvals?state=pending  ·  /approvals/{id}
GET /api/v1/sources · /schedules · /rules · /silences · /events · /events/{id}(trace)
GET /api/v1/secrets            → metadata only (name, kind, source, expiry, timestamps)
GET /api/v1/backups · /health  → doctor summary, one pass/fail per subsystem
GET /api/v1/commands           → self-describing command tree (use/short/example/flags)
```

Write endpoints (scope `operator`, every one enqueues):

```
POST /api/v1/workspaces/{name}/plan     → queue.Enqueue,   returns {job_id}
POST /api/v1/workspaces/{name}/apply    → queue.EnqueueGated, returns {approval_id, state:pending, ttl}
POST /api/v1/playbooks/{name}/run       → Enqueue (Gated if destructive/--require-approval)
POST /api/v1/commands                   → ad-hoc, injection-guarded (command.go:101-114)
POST /api/v1/sources/{name}/sync · /backups/run · /schedules …
```

Approval endpoints (scope `approve` or `operator`, `mayApprove` reused verbatim):

```
POST /api/v1/approvals/{id}/decision  {decision: approve|reject}  → reactive.Decide (fail-closed)
```

Conventions: every mutating request accepts an `Idempotency-Key` header →
`Spec.IdempotencyKey` → `JobByIdempotencyKey` dedup (queue.go:117-126), so a retry never
double-enqueues. The header is namespaced server-side by the authenticated token id and the
route before it reaches the queue: the dedup space is per-principal, per-verb, and the
queue's reserved `wf:` step keys are unaddressable from any face. `httpError`'s flat `{"error":msg}` (state.go) is replaced everywhere by the
`wire` typed envelope. Apply applies the **saved** plan, not a fresh one — the plan→apply seam
is two linked job rows and the approver sees the actual plan (overview.md:64-66).

### 3.5 MCP face (MCP over HTTP, `POST /mcp` on the Decision 0009 listener)

exocomp maintains exactly one programmatic MCP surface: MCP over HTTP, mounted at `POST /mcp`
on the same Decision 0009 listener as `/api/v1`, behind the same bearer+scope guard
(`internal/web/mcp_http.go`). It is a thin adapter that maps MCP tool calls to
`internal/service` verbs and marshals the identical `wire` envelope — the agent-aware wrapper
around the `/api/v1` bearer tokens, not a second server. Every MCP principal presents a scoped
token; there is no ambient-authority MCP path. Tool visibility is scope-gated: a `readonly`
token advertises only R tools.

The LOCAL agent path is the CLI, which is already agent-aware (`--output json`, the exit-code
table, the same `wire` envelope) — a local harness drives exocomp through the CLI, not through
an MCP server of its own. exocomp ships and maintains no stdio MCP server; a harness that must
speak stdio MCP runs an off-the-shelf http-to-stdio bridge (e.g. `npx mcp-remote` or
`supergateway`) around `POST /mcp` with a bearer token.

**Tools** (R = read, W = write/enqueues, A = approval-gated):

| Read (readonly) | Write (operator) | Approve (approve only) |
|---|---|---|
| list_sources, get_source | sync_source (W) | approve_job / reject_job (A) |
| list_workspaces, get_workspace | workspace_plan (W) | list_pending_approvals |
| get_drift (values-suppressed) | workspace_apply (A → gated) | |
| list_jobs, get_job | run_playbook (W/A) | |
| get_job_events / follow_job | run_command_adhoc (W/A) | |
| list_schedules, list_rules | backup_run (W) | |
| list_events, explain_event (Trace) | schedule_create/enable/disable (W) | |
| list_approvals, get_approval | secret_add/rotate/rm (W, value only IN) | |
| list_secrets (metadata only) | source/inventory/runsecret add/rm (W) | |
| list_backups, health/doctor | | |

**Never expose as tools:** `reveal_secret` / any plaintext read (no code path exists to call),
raw ciphertext, or an approval **decision link** as a return value — a decision URL is a
single-use HMAC capability, and returning one to a low-scope agent silently promotes it to
approver (reactive/approval.go; cmd/exocomp/approval.go:92-99). Approve/reject are scoped tools
that call `reactive.Decide` server-side instead.

**Every MCP write is attributed and answerable.** A write enqueued here records an actor
naming the `mcp` face, so an agent's action is never read off the ledger as the operator's
own CLI run; and `tools/call`/`resources/read` sent as a JSON-RPC *notification* are refused
with `invalid_request` rather than executed silently, because a mutation that writes no
frame back leaves an audit wrapper nothing to record.

**The A column is gated on the `approve` scope alone, and W and A are disjoint.** Approval
requires an explicitly minted `approve`-scope token; `approve_job`/`reject_job` are absent from
the catalog a lesser scope sees, and the scope that enqueues a gated apply may not settle it —
nor may any principal decide the approval for a job its own token asked for
(`docs/security/threat-model.md`). Because every MCP principal presents a scoped token and
none acts un-scoped, a face that raises a gate with `workspace_apply` can never also hold its
release; there is no ambient approver.

**Resources** (stable, cacheable context an agent fetches or subscribes to):
`exocomp://workspaces`, `…/workspaces/{name}/drift`, `exocomp://jobs/{id}`,
`exocomp://jobs/{id}/log` (append-only, `Seq`-cursorable), `exocomp://approvals/pending`,
`exocomp://schedules`, `exocomp://events/recent`, `exocomp://rules`, `exocomp://silences`,
`exocomp://secrets` (metadata), `exocomp://backups/status`, `exocomp://health`. Tools *operate*;
resources are the "be aware of" surface.

## 4. Auth and safety model

- **Reuse Decision 0009 tokens + three scopes.** `token new <name> --scope operator|readonly|approve`
  (store/token.go:13-17); argon2id-hashed at rest, revocable, minted only with the data dir on
  `server`. Scope maps 1:1 onto R / W / A across CLI, API, and MCP. A non-loopback bind still
  refuses without an active token (web/auth.go:250-268). An out-of-scope call is rejected with
  `forbidden` **before any store touch**.
- **Bearer failures back off, and the failure path is constant-cost.** Consecutive failures
  are counted per source and per token row; past a threshold the source serves a doubling
  backoff (429, `Retry-After`) that costs no KDF, and the run is logged once. An unknown or
  revoked token id pays the same argon2id a live one does, so response time is not an
  existence oracle.
- **A revoked token cuts an OPEN stream too.** The SSE tail re-resolves its token row on
  every poll and ends the stream when the row stops authenticating, with a hard 12h lifetime
  as backstop — a revoke is not something a caller can outrun by holding a socket open.
- **`/api/v1` writes take a bearer credential only.** A browser session cookie authenticates
  reads but is refused on every write route: an ambient credential rides whatever request a
  browser is talked into making, and SameSite does not scope to a port, so a second app on
  the same host could otherwise drive operator writes. The browser's own
  same-origin decide form is where a session acts.
- **W and A are disjoint on the machine face.** `/api/v1`'s decision route takes the `approve`
  scope alone: the token that may enqueue a gated apply must not also carry its release
  (`docs/security/threat-model.md`). The browser/phone surface keeps
  operator-or-approve — that is the human at the console — and both paths refuse a principal
  deciding an approval for a job its own token asked for. An operator driving a remote daemon
  with `approval decide --remote` needs an `approve`-scope token for that verb.
- **Cert pinning for the bare tailnet.** The listener already prints its self-signed cert
  fingerprint for trust-on-first-use (Decision 0009 §4). `exocomp login` performs the TOFU pin and
  stores `{host, fingerprint, token}` in `~/.config/exocomp/hosts.toml` (SSH-known-hosts
  discipline); later calls verify the pinned fingerprint. Where Tailscale Serve / Caddy+ACME
  fronts the surface, real TLS supersedes the pin. Operator-scope MCP/API over `--insecure-http`
  cleartext is disallowed — a bearer on the wire is sniffable (auth.go:262-266).
- **Approval gating is a queue state, fail-closed.** A destructive write goes straight into
  `awaiting_approval` via `EnqueueGated` — never queued first (queue/gate.go:11-51); no claim
  statement admits that state (store/job.go:18-53). The API/MCP write returns
  `{approval_id, state:pending, ttl, expires_at}`; an unanswered approval **auto-rejects** on
  TTL (SweepApprovals), and an approved plan refuses to apply if the workspace moved
  (`stale_plan`). Tools must surface `ttl`/`expires_at` so an agent reads a safety rejection as
  intended, not as an error.
- **Secrets are structurally non-exfiltrable.** No surface prints a sealed value; secrets rows
  hold only ciphertext/nonce/AAD; the Redactor scrubs raw/base64/hex **before** the append-only
  `job_event` write (invariant 4; internal/queue/redact.go; internal/secrets/redact.go). Secret
  tools return metadata only, and log/event tools read the already-redacted `job_event` — never
  re-open raw subprocess stdout for an agent.
- **Idempotency + concurrency are safe by construction.** Caller-settable idempotency keys
  dedup retries; the DB refuses a second apply per workspace via `concurrency_key`
  (queue.go:256-282). A retrying, flag-hallucinating small model cannot double-apply.
- **Kill switch is visible.** The global reactive silence (the one-row pause) is surfaced on
  `exocomp://health` / `/api/v1/health`, so an agent sees when the plane is paused.

## 5. Reconciling the design approaches

- **CLI `--json` first vs extract the service first, then all three faces.**
  These are sequenced, not opposed. The `wire` contract and CLI `--output json` land **first**
  because they are a pure presentation projection over existing store read models — cheap,
  network-free, and they force the schema and error taxonomy to exist and be golden-tested. The
  service extraction proceeds in parallel and is the prerequisite for the **API and MCP** faces,
  which must not fork logic. The CLI's JSON path and the API/MCP path converge on the one `wire`
  marshaller, so they stay byte-identical.
- **The caution about a general HTTP API** (risk of reintroducing the Decision 0001 bypass)
  is honored: the API is a thin reflection whose every write is `queue.Enqueue`
  / `EnqueueGated`. No endpoint shells out or mutates state directly; the queue remains the sole
  execution authority. The API also reuses the *existing* Decision 0009 listener and auth rather than
  standing up a second session system (which Decision 0001 explicitly rejected).
- **Where the operator API lives.** Not the loopback tofu backend (`internal/api`, which stays
  workspace-state-only) and not a new listener — the JSON routes mount on the token-gated web
  listener that already carries Decision 0009 auth, keeping one auth story.

## 6. Build order

Each step is a self-contained unit; the earlier steps ship value on their own and lock the
schema the later faces reuse. The `wire` contract and CLI JSON (step 1) do not depend on the
service extraction (step 2), so they land first.

1. **Contract foundation (no network).** The governing record + `internal/wire` envelope, error
   taxonomy, exit-code table + `--output json` on all read/list/status CLI verbs +
   `EXOCOMP_OUTPUT`. Golden tests pin the schema. cobra `Example` on every command +
   `exocomp commands --output json`. A local agent on `server` can already branch on JSON.
2. **Core + write ergonomics.** Extract `internal/service` from the `RunE`/store/queue logic
   (today a stub with no callers, so this is a rewrite, not a refactor). Surface
   `--idempotency-key` on mutating paths. Land the non-interactive contract
   (`--assume-yes`/`--no-input`, unify `--force`/`--yes-really`). doctor `--output json`/summary.
3. **Operator HTTP API (read).** JSON read endpoints on the Decision 0009 listener over the service
   verbs, typed `wire` errors, OpenAPI 3.1 spec + `internal/client`. SSE job-follow.
4. **Remote CLI + write API.** `exocomp login` (TOFU pinning) + `--remote`/`EXOCOMP_REMOTE`
   client mode; write/approval endpoints (plan/apply/run/decide) through queue+gate.
   `workstation → tailnet → server` works end to end.
5. **MCP face.** `POST /mcp` (MCP over HTTP) adapter over the service verbs, same envelope;
   tool/resource catalog with scope gating, on the Decision 0009 bearer+scope guard. The local
   agent path is the agent-aware CLI; exocomp ships no stdio MCP server (a stdio client bridges
   to `POST /mcp` with an off-the-shelf http-to-stdio wrapper).

## 7. Risks and open decisions

- **Charter scope.** No existing design doc names an agent/MCP/remote-CLI consumer as a
  requirement; the original operator-centric charter did not name agent-fit as first-class. Decision 0014
  records that decision and its one resolved design question — a single MCP surface over HTTP
  with no ambient authority — so the surface is a decision with consequences, not an assumption.
- **Schema is a contract.** If the CLI and API/MCP envelopes ever diverge, or a field shape
  changes without a `schema_version` bump, agent parsers and prompt caches break silently. One
  shared `wire` marshaller under golden tests is the mitigation.
- **Presentation-only JSON.** The envelope must never leak into persistence — invariant 2 bans
  bespoke stored formats. It is a projection of store rows, never a stored artifact schema.
- **Stale examples mislead.** cobra `Example` blocks and the command dump must be asserted in CI
  (flags exist / examples parse) under `make check`, or they rot into active misdirection.
- **Redaction on the new paths.** Every JSON output path must route through the same sink-time
  redactor and the tofu backend's internal-hiding `fail()` discipline, or a verbose small model
  re-exposes what invariant 4 protects.
- **Carve-outs stay read-only.** `restore` is deliberately outside the queue
  (overview.md:77-80). MCP marks such carve-outs read-only rather than inventing a queued
  path the core does not have.

## 8. Evidence

- docs/architecture/overview.md:93-104 — one core, thin clients; `internal/service` owns all
  business logic; OpenAPI 3.1 as the monorepo seam; TUI/web/CLI/webhooks as faces.
- internal/service/doc.go — service layer is still a stub; logic lives in `RunE`/store/queue.
- internal/api/server.go:32-37,55-60,100-125 — tofu backend binds loopback only; deterministic
  HKDF workspace bearer; `Mount` allows extra routes on the one listener; `/healthz`.
- internal/api/state.go:51-235 — JSON backend; flat `{"error":msg}` via `httpError`; `fail()`
  hides internals from job subprocesses.
- internal/web/server.go:58-60,121-148,174-187 — web reads the store directly for one auth
  story; the mux behind `requireAuth`; strict CSP/security headers (the seam the JSON API rides).
- internal/web/auth.go:41,173-268 — Decision 0009 resolve (session→bearer), `mayApprove`, KDF gate
  (`ErrKDFBusy`), refuse non-loopback bind with no active token, `--insecure-http` warning.
- internal/store/token.go:13-45,136-146 — three scopes; argon2id at rest; revoke;
  `CountActiveTokens` (the bind guard).
- internal/queue/queue.go:112-161,256-282,357-379 — `Enqueue` idempotency dedup; concurrency
  serialization decided by the store; exit-code → state mapping (invariant 3).
- internal/queue/gate.go:11-51 — `EnqueueGated` writes destructive work straight into
  `awaiting_approval`, never queued first.
- internal/store/job.go:18-53,209-217 — job state machine, `awaiting_approval` claimable by
  nothing; `JobFilter{States,ConcurrencyKey,Runbook,Limit}` for the list endpoint.
- internal/executor/tofu_drift.go:9-31 — `ResourceDelta`/`TofuPlanOutcome` suppress values,
  emit keys/actions/counts (the token-efficient contract template).
- internal/web/stream.go:16-90 — SSE tail over append-only `job_event`, monotonic `Seq` cursor,
  `done` on terminal (the follow_job / `/stream` model).
- internal/store/store.go:20-31 + internal/reactive/approval.go:27 — the sentinel set the error
  taxonomy maps onto (`ErrNotFound`/`ErrStateConflict`/`ErrDuplicate`/`ErrConcurrencyHeld`/
  `ErrStalePlan`).
- internal/queue/redact.go + internal/secrets/redact.go — Redactor scrubs raw/base64/hex before
  the append-only write (invariant 4).
- cmd/exocomp/workspace.go:432-436 — plan runs `-detailed-exitcode`, exits 2 on changes while
  the job succeeds (exit-code 2 in the table).
- cmd/exocomp/command.go:46-47,101-114 — `--require-approval` gating + injection guard on ad-hoc
  runs.
- cmd/exocomp/serve.go:109-128 — `--port` loopback state backend vs `--web-addr` token-gated LAN
  web+API surface over TLS; `--web-insecure-http`.
- docs/decisions/0009-lan-access-tokens.md:15-35 — bearer required for non-loopback; mint via local
  auth; TLS + printed fingerprint for TOFU pinning; audit records token name.
- docs/architecture/platform-contract.md:47-49 — the tailnet is the intended remote path (wizard
  binds the TCP listener to the tailnet address).
- AGENTS.md invariants 1-5 — one operator/container/no external DB; no bespoke stored formats;
  exit codes decide correctness; secrets never in logs/events; every path through the one queue.
