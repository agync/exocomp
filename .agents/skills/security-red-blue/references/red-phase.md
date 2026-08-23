# Red phase — refute the surface

The red phase produces working exploits, not a review. It ends with a ranked
finding list; the ones marked `exploited` carry a repro that blue turns into a
regression test.

## §1 Build, freeze, ground truth

- Build a real binary (`make build` → `bin/exocomp`, or `go build -o /tmp/... 
  ./cmd/exocomp`). Init a throwaway data dir under a scratch `XDG_DATA_HOME` in
  a `mktemp` dir. Attack that instance; never sudo, never touch tracked files,
  never reach networks beyond what a normal run does.
- The surface under test is the three programmatic faces ADR 0014
  (`docs/adr/0014-agent-facing-surface.md`) ratifies:
  - the **write half of `/api/v1`** — the write routes in `internal/web`
    (`api_write.go`, `mcp_http.go`) with bearer scopes minted per token;
  - the **remote CLI** — `exocomp login` / `--remote` (pin + bearer);
  - the **MCP server** — the stdio face (`internal/mcp`) and its HTTP mount
    (`internal/web/mcp_http.go`).
- Establish ground truth yourself from source and docs: what each route/tool
  actually does, which scope reaches it, whether a write enqueues or gates,
  which principal the ledger records. The attacker's *belief* about a guard is
  the thing under test, so it can never stand in for the truth.

## §2 Attack classes to try

Derive concrete exploits in each class, then execute them. These are the classes
the `AI-*` wave found real holes in — treat them as the minimum surface, not the
ceiling.

- **Authority / scope.** Can one credential both raise a gate and release it?
  Does a scope reach a tool it is not advertised (per-call check, not just
  `tools/list` filtering)? Does the browser session leak write authority a
  machine token should carry alone?
- **Approval gate.** Can the principal that enqueued a gated job settle its own
  approval? Does an expired or unpinned decision link still release? Does a
  forged resolve cancel a pending approval?
- **Plan-policy adjudication.** Is there a window where a plan reads "no verdict"
  as "no policy" and applies unpoliced? Does a fresh apply skip policy that only
  the plan→apply-by-id path consults? Can a plan apply in a workspace it was
  never made against?
- **Idempotency / attribution.** Is an `Idempotency-Key` a global namespace a
  planted key can cross? Does a write record the wrong actor, or a notification
  mutate state while writing zero bytes back?
- **Session / CSRF.** Are cookie sessions accepted on write routes with no
  bearer, Origin, or Content-Type check?
- **Path confinement.** Can a `--source` playbook escape its checkout via `../`
  or an absolute path, so a run recorded against a signed source executes an
  uncovered tree?
- **Auth timing / rate.** Is there a lockout or backoff on bearer auth? Does the
  unknown-token path return early and leak a timing oracle for which ids exist?
- **Stream lifetime.** Does a revoked token keep receiving an already-open SSE
  stream because auth was resolved once, at attach?

## The finding shape

Each finding is the object `.agents/workflows/post-red-team.js` mandates:

- `title` — one line naming the hole.
- `severity` — critical/high/medium/low/info, as solo-operator risk.
- `exploited` — bool: `true` only when reproduced against a real binary;
  otherwise the repro field says `ANALYSIS-ONLY`.
- `vector` — the entry point and precondition.
- `repro_or_analysis` — the concrete steps and what happened, with the guard
  cited at `file:line` so blue can find it.
- `operator_risk` — realistic risk to a solo homelab operator, and the
  preconditions an attacker needs.
- `mitigation` — the fix direction (blue refines it into the actual guard).

When a finding is promoted into `docs/security/threat-model.md`, the table row
carries `id` (`AI-N`), the one-line finding, `exploited`, and a **disposition**:
FIX (blue-team wave), HARDEN (defense-in-depth), ACCEPT (documented residual),
or DOC (honesty correction). Only FIX findings become a failing-then-passing
test; HARDEN/ACCEPT/DOC are recorded, not necessarily tested.

## Ranking

Rank by what a real solo operator running one container and host loses, not by
CVSS. A dangerous quiet failure — a gate that silently opens, a policy that
silently does not run, an attribution written onto a foreign ledger — outranks a
loud one. Report most-severe first; an honestly-empty list beats an invented
finding.
