export const meta = {
  name: 'exocomp-m3b-surfaces',
  description: 'M3b: source+credential model, web UI+LAN auth, TUI screens+metrics+wizard',
  phases: [{ title: 'Build', detail: 'sources (opus) + web (opus) + ops-ui (opus)' }],
}
const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args
const WT = REPO + '/.worktrees'
const COMMON = `
Read <worktree>/AGENTS.md and <worktree>/.agents/workflows/m1-plan.md (final "M3b" section = your ownership map + gate) FIRST. Also BINDING: docs/design/v1-acceptance.md (the five stories), docs/design/expert-reviews.md (v1 must-fixes + how practitioners run each story), docs/adr/0009 (LAN tokens), docs/adr/0010 (source model + 3 localities), docs/design/ui-brief.md ("Approval surface contract"), docs/design/tui-brief.md, internal/web/tokens.css, internal/tui/testdata/frames (golden fixtures). Conventions: lane-owned paths only; logical-unit commits via "git -C <worktree>"; conventional titles <= 52 chars; NO trailers; explicit staging; no slop comments; never sudo. Pre-commit runs make check (go test -race ./..., ~2.5 min) on the index — never bypass; be patient. REUSE: secrets Envelope/Derive/Redactor, store migration+CRUD patterns, queue Options+gate, executor Driver + the sandbox tier (jobs run sandboxed — new run env must be granted, not leaked), reactive engine, sched. Subprocess success is exit-code-only; secrets never in argv/env-dumps/events/persisted-plaintext. Verification with REAL output; mutation-check guards.
Host: go 1.26, golangci-lint, tofu, ansible-playbook+ansible-galaxy, restic, age, sops, podman, limactl, nono 0.74.0. Never sudo.
`
const SCHEMA = {
  type:'object', required:['commits','summary','verified','concerns'],
  properties:{
    commits:{type:'array',items:{type:'object',required:['sha','title'],properties:{sha:{type:'string'},title:{type:'string'}}}},
    summary:{type:'string'}, verified:{type:'array',items:{type:'string'}}, concerns:{type:'array',items:{type:'string'}},
  },
}
phase('Build')
const results = await parallel([
  () => agent(`${COMMON}
YOUR LANE: sources. Worktree: ${WT}/m3b-sources. This is the spine of v1 — the credential plane that makes the five stories real. Own: internal/source/**, migration 0006, cmd/exocomp/{source.go,host.go}, the RUN-ENV CREDENTIAL BINDING in internal/executor/{ansible,tofu}.go, ansible/tofu flag passthroughs, workspace add --state-only, mode-A emit, drift-detail, offline lock-hash fix, tfstate pruning.

Build, grounded in expert-reviews.md's per-story v1 minimums:
1. Migration 0006: source(id,name UNIQUE,kind[git|hostpath|volpath],uri,ref,deploy_key_secret,last_sync_sha,signed,enabled), inventory(id,name,source_id,path,kind[static|dynamic],env_binding_json), run_secret binding (a named sealed-secret set → env names for a run). Store CRUD, new files only.
2. internal/source: git register (generate ed25519 deploy key sealed in the envelope, print the public half), clone into the data volume, sync-to-pinned-ref, verify a signed tag; hostpath + volpath localities (ADR 0010) — no key, tree already present. A source run works from the synced/mounted checkout with spec.Dir = checkout root (fixes the ansible.cfg-not-loaded bug from the expert review).
3. THE CONVERGENT MUST-FIX — credential-plane binding: a sealed-secret set bound to a run, exported as NAMED ENV into the job (provider env for tofu, plugin env for ansible dynamic inventory, TF_VAR_). This is what the sandbox allowlist currently strips — so the binding must ADD the specific env names to the driver's env allowlist for THAT run (not widen the global allowlist). Prove a dynamic inventory plugin (community.general.proxmox or a fake plugin needing an env token) authenticates from a bound secret UNDER THE SANDBOX.
4. Ansible fidelity (expert must-fix): flag passthrough --check/--diff/--limit/--tags/-e via a run "-- <args>" form; --check as first-class (Ansible's plan); multiple --vault-id labels; import an existing vault password (not only HKDF-derived). Point ANSIBLE_INVENTORY cache at a backed-up data-dir path with a cache_timeout knob for dynamic inventory.
5. Tofu fidelity (expert must-fix): workspace add --state-only emits a ready-to-paste backend "http" block + bearer (reuse WorkspaceToken + the existing backend-config writer), with the workspace-collision note in the emitted comment (one exocomp workspace = one state; tofu workspace new/select unsupported); arg passthrough -var-file/-target/-replace; drift via plan -refresh-only -detailed-exitcode storing resource_drift[].address + attr KEYS (values suppressed) not a bare bool; offline lock-hash fix (tofu providers lock -fs-mirror writes platform-correct h1: entries so offline init verifies); a tfstate pruning policy (keep last N + last M days) as a doctor/backup concern.
6. CLI: source add|list|sync|rm (git + --path localities), host onboard (seeded runbook: push fleet key, create user, sudoers, record known_hosts stored+backed up), inventory add binding secrets, the run forms above.
Gate (prove on the lima node where the story needs a host): register a git repo with a generated deploy key + signed ref; run a play FROM the checkout with a DYNAMIC inventory authenticating from a bound sealed secret under the sandbox; ./ansible.cfg loads; two vault-ids resolve; a hostpath source runs. tofu: workspace add --state-only emits a block an UNMODIFIED external tofu uses (state lands encrypted+locked); mode B runs a provider needing a bound token; -var-file passes; drift reports resource addresses. Paste real transcripts.`,
    { label:'lane:sources', phase:'Build', model:'opus', schema:SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: web. Worktree: ${WT}/m3b-web. Own: internal/web/** (templ+htmx), ADR 0009 LAN auth, cmd/exocomp/{web.go,token.go}. Deps: a-h/templ; no JS framework.

1. LAN auth per ADR 0009: cmd/exocomp/token.go — token new <name> [--scope operator|readonly|approve] [--expires], list, revoke; argon2id-hashed in the secret store; lifecycle columns. serve refuses a non-loopback bind unless a token exists; self-signed TLS cert generated in the data dir 0600 on first LAN bind, fingerprint printed with the token + in doctor. Browser session: token → HttpOnly SameSite=Strict cookie bound to the token row (revoke kills sessions). Every gate assertion in ADR 0009.
2. internal/web: templ + htmx on internal/web/tokens.css (BOTH themes first-class, tabular-nums, the split accent, focus-visible ring — a11y hard reqs from ui-brief.md). Screens: dashboard (banner states pause/silence/both, needs-you, queue with oldest-queued age, backup freshness incl. drill row, workspaces drift, footer signal line dead-man/ntfy/disk/version), jobs list + live tail via SSE, phone approval at 390px (the seven-point approval-surface contract from ui-brief.md — risk-ordered, destroys first, [redacted·secret], fail-closed TTL, stale-plan refusal, mute with mandatory expiry + hold-to-confirm global kill), events + evaluation trace. Read plan artifacts through the sealed-artifact path (secfolds encrypted them — never read ciphertext raw).
3. cmd/exocomp/web.go: mounts the web UI on the same listener as the API, token-gated.
Gate: token-gated LAN listener (tokenless bind refused, revoked token rejected, session dies with token, --insecure-http warns); dashboard + phone approval render on tokens.css in both themes; approve-a-gated-job from the phone surface runs the job (drive it headless: create a gated job, hit the approval endpoint through a session cookie, confirm it runs); keyboard-operable + visible focus ring asserted. Paste real output incl. an HTML snippet showing the tokens/a11y attributes.`,
    { label:'lane:web', phase:'Build', model:'opus', schema:SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: ops-ui. Worktree: ${WT}/m3b-ops-ui. Own: internal/tui/** (new screens + banner + keymap), internal/metrics/**, cmd/exocomp/setup.go, doctor self-observability lines.

1. TUI new screens matching the golden fixtures in internal/tui/testdata/frames (your View() output must diff clean against them — home-80x24, home-120x36, approval-detail-80x24, events-120, trace-80, banners-80, mute-modal-60, stale-plan-refusal-80, destroy-confirm-80, wizard-*, empty-workspaces): Home[0] (glance parity), Approvals[5] (the approval contract terminal-rendered: destroys first, [redacted·secret], a/r, stale-plan refusal, destroy-approval confirmed by typing the workspace name), Events[6] (outcome enum + reason, e opens the evaluation trace). Banner system on every screen (PAUSED vs MUTED — pause holds reactive jobs, mute holds notifications while jobs run; p pauses from anywhere; mute modal mandatory expiry). Keymap deltas: q quits only at root (esc walks back), [0] is Home. Follow tui-brief.md + colors.txt (16-color fallback) + copy.txt (load-bearing strings verbatim; {braces}=computed).
2. Notification-mute (the engine gap the TUI needs): a notify-layer mute (scope + mandatory expiry + logged reason) distinct from the reactive silence/pause — yields the MUTED banner. Add to internal/notify + a store row; cmd exocomp mute add/list/rm.
3. internal/metrics: a /metrics endpoint (Prometheus text) exposing STALENESS GAUGES (last-success timestamps: per backup leg, last drill, last drift check per workspace, oldest queued job age, pending approval age, dead-man ping age) + a seeded alert-rules file covering reactive-paused, approvals-backing-up, backup-stale-48h, drill-failed, disk-low, queue-stuck. Mount on the listener.
4. cmd/exocomp/setup.go: the 4-step wizard (identity→recovery kit→first backup+restore drill→hand over) as the exocomp setup flow — NOTHING written to disk until step 4; the kit step blocks on the typed confirmation; operator name + generated ntfy topic + test push at step 4. Idempotent, check-before-create.
5. doctor: add self-observability lines (metrics reachable, dead-man configured, silence/mute active).
Gate: the 3 new TUI screens' View() diffs clean vs the golden fixtures (assert substrings if exact-match is brittle, but aim exact); /metrics scrape shows the staleness gauges; exocomp setup runs end-to-end writing nothing until step 4 (drive it scripted); a notification-mute yields MUTED distinct from PAUSED. Paste real output.`,
    { label:'lane:ops-ui', phase:'Build', model:'opus', schema:SCHEMA }),
])
return { lanes: results.filter(Boolean) }