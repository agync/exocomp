export const meta = {
  name: 'exocomp-m2b-surfaces',
  description: 'M2b: serve unification, TUI expansion, offline caches + fixture swap',
  phases: [
    { title: 'Build', detail: 'serve (opus) + tui (sonnet) + caches (sonnet)' },
  ],
}

const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args
const WT = REPO + '/.worktrees'

const COMMON = `
Read <worktree>/AGENTS.md and <worktree>/.agents/workflows/m1-plan.md — the "M2b" sections at the end are your ownership map and gate. Conventions: lane-owned paths only; logical-unit commits via "git -C <worktree>"; conventional titles <= 52 chars; NO trailers; explicit staging; no slop comments; never sudo. Pre-commit runs make check (~90s, includes go test -race ./...) on the index; never bypass it. REUSE existing building blocks — read them first. Subprocess success is exit-code-only; secrets never in argv/env-dumps/events/files outside 0700. Verification with REAL output.
Host: go 1.26, tofu 1.11.5, ansible-playbook + ansible-galaxy (uv-installed), restic, age, sops, podman, limactl, unshare available (user namespaces). Never sudo.
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
YOUR LANE: serve-unify. Worktree: ${WT}/m2b-serve (branch m2b/serve). Own: cmd/exocomp/serve.go, cmd/exocomp/schedule.go, plus a serve integration test file.

1. Make "exocomp serve" THE daemon: state backend (as now) + queue workers + scheduler (internal/sched) + SweepInterrupted at startup, one process. Shutdown ordering on SIGINT/SIGTERM: stop accepting cron fires -> cancel queue with a grace window -> in-flight backup/tofu legs finish their terminal DB writes (the engine already uses context.WithoutCancel for finalization — verify end-to-end that a SIGTERM mid-backup leaves the backup_run and job rows terminal, not running; fix in serve's shutdown ordering if not, without touching engine files).
2. "exocomp scheduler run" becomes a deprecated alias printing a pointer to serve (keep it working this milestone).
3. "exocomp schedule enable <name>" / "disable <name>" subcommands over the existing store.SetScheduleEnabled + a Reload trigger note (serve picks up changes on its reload interval or SIGHUP — implement SIGHUP -> sched.Reload()).
4. Integration test (build the binary, drive it as a process): serve starts, healthz answers, a @every 1s schedule fires jobs while a workspace apply runs concurrently (use the stand-in tofu script pattern from internal/executor/tofu_test.go if the real binary is too slow), SIGTERM mid-backup (slow fake target via a large-ish dir or a shell schedule sleeping) leaves zero rows in state running/queued that the boot sweep then has to fix — assert directly post-exit.
Gate: make check; the integration test's real output; a manual serve transcript showing backend + scheduler + SIGHUP reload + clean SIGTERM.`,
    { label: 'lane:serve', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: tui-expand. Worktree: ${WT}/m2b-tui (branch m2b/tui). Own: internal/tui/**, cmd/exocomp/tui.go.

Add two screens to the existing Bubble Tea model (read internal/tui/model.go first; follow its established message/tick/table patterns exactly):
1. Workspaces [3]: name, serial, last plan job state, last apply job state, and a drift marker derived from the most recent plan job's tofu outcome event (RawJSON {"tofu":{"operation":"plan","changes":true}} — read via the store's job_event queries; executor.TofuPlanChanges may help). Enter opens the existing event-tail viewport for the selected workspace's latest job.
2. Schedules [4]: name, cron, kind, enabled (dim disabled like backups does), next_at, last job state.
3. Update the help bar and [1]-[4]/tab cycling; keep the styling discipline (one accent, no emoji).
4. Tests following internal/tui/model_test.go patterns: store-backed rendering of both new screens (seed workspaces + tfstate + plan/apply jobs with outcome events; seed schedules incl. disabled), navigation, drift marker shown only when the last plan reported changes.
Gate: make check; go test ./internal/tui/ -race -count=1; a store-backed View() dump showing all four screens render.`,
    { label: 'lane:tui', phase: 'Build', model: 'sonnet', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: caches+fixture. Worktree: ${WT}/m2b-caches (branch m2b/caches). Own: the TF_PLUGIN_CACHE_DIR lines in internal/executor/tofu.go, ANSIBLE_COLLECTIONS_PATH lines in internal/executor/ansible.go, cmd/exocomp/vendor.go, the fixture swap in internal/backup/s3_test.go + test/e2e/m1-gate.sh, docs/operations/** + docs/log.md entries for what you change.

1. Offline caches (docs/design/amendments.md item on offline-restorable dependencies): tofu driver sets TF_PLUGIN_CACHE_DIR to <dataDir>/cache/tofu-plugins (created 0700) so provider downloads cache across runs; ansible driver appends <dataDir>/cache/collections to ANSIBLE_COLLECTIONS_PATH. "exocomp playbook vendor --requirements <file>" runs ansible-galaxy collection install -r into that cache (exit-code correctness, events to stdout).
2. Offline proof test: vendor a small collection (community.sops or ansible.utils) into a scratch cache, then run a playbook using it with the network denied — prefer "unshare -rn" (user+net namespace, no sudo; verify it works on this host first) around ansible-playbook; if unshare is blocked, point ANSIBLE_GALAXY_SERVER and registry endpoints at 127.0.0.1:9 and assert success purely from cache. Same for tofu: after one online init (local_file provider), a second init/plan in a network-denied namespace must succeed from the plugin cache.
3. Fixture swap per docs/sbom/minio-assessment.md: replace the MinIO container in s3_test.go and the e2e gate with SeaweedFS (docker.io/chrislusf/seaweedfs, "weed server -s3", note its S3 port and credential config — SeaweedFS S3 auth needs a config or -s3.config; anonymous mode is acceptable for the fixture if restic accepts it with dummy creds, verify). Run the S3 tests AND the full e2e gate for real against the new fixture. If restic or the SigV4 root-placement breaks against SeaweedFS in a way you cannot fix in fixture config, KEEP the pinned MinIO image (pin by digest), document exactly what broke in docs/sbom/minio-assessment.md, and say so in your report — do not force it.
4. Operator docs: document the s3/<target>/access-key-id + secret-access-key sealed-row naming in docs/operations/ (the backup lane shipped the mechanism with no operator-facing doc), and the vendor + cache behavior; docs/log.md entries.
Gate: make check; the offline-proof test output (network-denied play + plan succeeding); S3 tests + e2e gate output against the final fixture choice.`,
    { label: 'lane:caches', phase: 'Build', model: 'sonnet', schema: SCHEMA }),
])

return { lanes: results.filter(Boolean) }