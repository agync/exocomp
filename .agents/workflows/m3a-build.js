export const meta = {
  name: 'exocomp-m3a-core',
  description: 'M3a: reactive engine, sandbox tier, security folds',
  phases: [{ title: 'Build', detail: 'reactive + sandbox + secfolds, all opus' }],
}

const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args
const WT = REPO + '/.worktrees'

const COMMON = `
Read <worktree>/AGENTS.md and <worktree>/.agents/workflows/m1-plan.md (final "M3" section = your ownership map + gate) FIRST. Also binding: docs/design/ui-brief.md "Approval surface contract" section, docs/design/user-feedback.md "Folded into M3" list, docs/architecture/execution.md, ADR 0005/0006/0008/0009. Conventions: lane-owned paths only; logical-unit commits via "git -C <worktree>"; conventional titles <= 52 chars; NO trailers; explicit staging; no slop comments; never sudo. Pre-commit runs make check (go test -race ./..., ~2.5 min) on the index — never bypass. REUSE existing packages (queue Options, secrets Envelope/Derive/Redactor, store patterns, executor Driver, sched); read before writing. Subprocess success is exit-code-only; secrets never in argv/env-dumps/events/persisted files outside the envelope. Verification with REAL output; mutation-check your central guards.
Host: go 1.26, golangci-lint, tofu, ansible-playbook, restic, age, sops, podman, limactl, nono 0.74.0 on PATH. Never sudo.
`

const SCHEMA = {
  type: 'object',
  required: ['commits', 'summary', 'verified', 'concerns'],
  properties: {
    commits: { type: 'array', items: { type: 'object', required: ['sha', 'title'], properties: { sha: { type: 'string' }, title: { type: 'string' } } } },
    summary: { type: 'string' },
    verified: { type: 'array', items: { type: 'string' } },
    concerns: { type: 'array', items: { type: 'string' } },
  },
}

phase('Build')
const results = await parallel([
  () => agent(`${COMMON}
YOUR LANE: reactive. Worktree: ${WT}/m3a-reactive (branch m3a/reactive). Dep allowed: google/cel-go.

Build the reactive core per docs/architecture (event -> rule -> guardrails -> approval -> queue) and the approval surface contract:
1. Migration 0005: event(id, source, received_at, dedup_key, normalized_json, raw_json, matched_rule, outcome, reason), rule(id, name UNIQUE, source_filter, cel_expr, action_kind, action_args_json, cooldown_s, daily_cap, require_approval, enabled), rule_fire(rule_id, ts, job_id), silence(id, scope_kind /alert|host|global/, scope_value, reason, created_at, expires_at NOT NULL, created_via), approval(id, job_id, plan_artifact, requested_at, expires_at, token_hash, state /pending|approved|rejected|expired|withdrawn/, decided_at, decided_via, reason). Store CRUD in internal/store (new files only).
2. internal/reactive: webhook ingress handlers on the existing api server (POST /hook/{source-token}, HMAC-signed via Derive("webhook-hmac", source), CIDR allowlist optional) for alertmanager + healthchecks + generic JSON; normalizer to one event shape; cel-go evaluation (sandboxed, time-budgeted, compile-at-load with errors surfaced at rule add); guardrails ENFORCED AT ENQUEUE in ledger order: silence match (global pause = a global-scope silence; swallow + count), dedup window, cooldown (rule_fire query), daily cap (cap hit DISABLES the rule + max-priority notify), require_approval or destructive action -> create approval instead of running. Outcomes persisted for EVERY event with reason strings matching the events design ("cooldown 6h, next eligible HH:MM"). An explain function returning the per-condition evaluation trace for a stored event (CLI: "exocomp event explain <id>").
3. Approvals: HMAC single-use tokens (Derive("approval")), TTL fail-closed (expiry -> job rejected + notified), resolve-cancels (alert resolved while pending -> withdrawn + notified), stale-plan refusal for tofu applies (workspace serial moved -> refuse with the moved-by attribution, per the contract). Decision endpoints on the api server (constant-time, single-use, terminal-notice on reuse). approve path enqueues through the EXISTING queue gate: add internal/queue/gate.go so a job created awaiting_approval only becomes claimable on approval (state machine already reserves awaiting_approval).
4. internal/notify: ntfy transport (self-hosted URL + access token from sealed secrets; refuse a bare ntfy.sh topic with an error naming the policy), action buttons carrying the decision URLs, priorities, and a re-publish at 50% TTL. Interface seam for more transports.
5. CLI: rule add/list/enable/disable (CEL compile-checked at add), silence add/list/rm (expiry MANDATORY, global scope allowed with --yes-really + expiry), approval list, event list/explain. serve wires ingress + expiry sweeps + resolve watching.
6. Scheduler: add schedule kind "tofu-plan" (drift detection: plan exits 2 -> event "drift" into the same pipeline, so drift can gate an apply through approval).
7. Tests (-race, mutation-check the guards): full pipeline with a fake alertmanager POST through to a queued-then-gated job; every guardrail individually (incl. cap-disables + crash-survives-ledger via reopen); silences incl. global + expiry; approval happy/expire/reuse/withdraw/stale-plan paths; ntfy against an httptest server incl. token refusal; explain trace golden-ish assertions.
Gate: make check; the pipeline test outputs; a REAL end-to-end demo in a scratch data dir: serve up, curl an alertmanager payload, watch it gate, decide via the token URL (curl), job runs through the daemon, event list shows the funnel; paste that transcript.`,
    { label: 'lane:reactive', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: sandbox. Worktree: ${WT}/m3a-sandbox (branch m3a/sandbox). No new Go deps.

Implement ADR 0005's local-sandboxed tier and ADR 0006's image toolchain:
1. internal/executor/sandbox.go (+_test): a driver wrapper running any executor command under "nono run" with per-kind profiles: allow the job's private dir, repo checkout/workdir, tool binaries, and the caches the drivers use (tofu plugin cache, collections path); DENY the data dir (exocomp.db, host.key, keyring.age, serve.lock, state-backend.addr) and all unix sockets. Tier selection: config/flag on serve + per-driver constructor option; probe at startup (run a canary under nono asserting a read of host.key FAILS and a socket connect FAILS) -> on probe failure or missing nono/Landlock, degrade LOUDLY to plain local (warning + doctor line), never silently. doctor gains: nono version+floor(>=0.55), Landlock ABI, tier in effect, probe result.
2. Network policy: ansible jobs need SSH egress; tofu needs HTTPS + the loopback state backend — express what nono 0.74 supports (check its flags: --allow-net forms); if per-destination filtering is impractical, allow-net for those kinds and say so in the report + a doc note for the integrator.
3. Containerfile (ADR 0006): a real toolchain layer — uv python install (pinned CPython) + uv tool install ansible-core==<pin> --with ansible-runner -c constraints.txt (write the constraints file, pin currently-installed host versions), plus pinned tofu/restic/age/sops/git binaries fetched by sha256 into the image; UV_TOOL_DIR=/var/lib/exocomp/toolchain note per ADR 0006 (image default stays baked-in). Multi-stage, final image runs the daemon as before. Budget targets in the Makefile: "make image" (podman build) and "make budgets" printing image size + idle RSS of ./bin/exocomp (start serve against a scratch dir, sample RSS, stop) with the ceilings from docs/sbom (800MB / 80MB) as FAILING thresholds.
4. Tests: sandbox profile unit tests (argv construction); the REAL probe test under nono (it is on PATH — must run: sandboxed cat of a fake host.key fails, plain local succeeds); an ansible playbook and a tofu plan run E2E under the sandboxed tier proving normal jobs still work; packaging/image_test extended: podman build succeeds AND "podman run --rm <img> exocomp doctor"-style probe shows the baked toolchain (ansible-playbook found in-image).
Gate: make check; the real nono probe output; sandboxed ansible+tofu e2e output; make image + make budgets REAL numbers.`,
    { label: 'lane:sandbox', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: secfolds. Worktree: ${WT}/m3a-secfolds (branch m3a/secfolds). No new deps.

Security folds from the persona review + design handoff (read docs/design/user-feedback.md items 1-2, docs/log.md's design-handoff reconciliation notes, and the recovery-kit design docs/design/reference/canvas/recovery-kit.png context via its description in docs/log.md):
1. Keyring versioning: the keyring file gains a monotonic version (v1 = existing files, parse both); rotation/recipient changes bump it; RESTORE REFUSES a keyring older than the version recorded in the backup manifest (downgrade = the rollback attack); kit prints "kit vN, replaces vN-1 (destroy it)".
2. Manifest authentication: HKDF subkey Derive("backup-manifest-mac", target) HMACs MANIFEST.json and the git leg's parts manifest; restore verifies before trusting hashes; corruption vs tampering distinguished in the error. Old (un-MAC'd) backups restore with a LOUD "legacy backup: integrity unverified" warning, never silently.
3. cmd/exocomp/kit.go: "kit derive <purpose>/<id>" (offline: asks only for recovery key + salt inputs when the envelope is absent, else uses it) matching the card's grammar incl. ansible-vault/<project> and restic/<target>; "kit derive --list" enumerating every purpose:id in use from the store; "kit verify" recomputing the printed page's sha256 from the daemon's current kit rendering and comparing; "identity migrate" moving the host key storage (file <-> re-wrap for new recipients) with the kit reprint nag. Card rendering (internal/secrets/kit.go) updates to the handoff's SECTION LAYOUT (numbered sections incl. hand-written credential box, kit version line, page sha) but derivation text RENDERS FROM CODE (constant salt exocomp/v1, info purpose:id — NOT the mock's per-install salt; docs/log.md records this reconciliation) and commands render as exocomp (never exo, ADR 0002). Extend kit_test: card still executes verbatim incl. a vault-id derivation proven against ansible-vault, and kit verify round-trips.
4. Plan artifact encryption at rest: tofu plan artifacts (and any artifact marked sensitive) age-encrypted to the envelope recipients on write, decrypted transparently for the approval diff path; restore drill unaffected (artifacts are not in the backup staging tree — verify and state so).
5. DCO: add the standard DCO file at repo root + one AGENTS.md line under "Working here" (sign-off NOT required in commit messages — record-keeping only per the monetization-direction memory; do not add trailers).
6. Tests: keyring downgrade refused (craft an older-version keyring at a target root); tampered manifest refused with the tampering error; legacy un-MAC'd backup warns; kit derive == envelope derivation for 3 purposes; kit verify catches a flipped byte; migrate round-trip; encrypted plan artifact unreadable raw + readable via the approval path.
Gate: make check; the downgrade/tamper refusal outputs; kit derive cross-check vs a real restic repo open; the updated card executed end-to-end (kit_test) output.`,
    { label: 'lane:secfolds', phase: 'Build', model: 'opus', schema: SCHEMA }),
])

return { lanes: results.filter(Boolean) }