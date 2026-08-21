export const meta = {
  name: 'exocomp-m2c-fixes',
  description: 'M2c: cross-lane daemon/queue fixes in one lane',
  phases: [{ title: 'Fix', detail: 'single opus lane, interlocking queue/cmd/engine items' }],
}

const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args

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

phase('Fix')
const result = await agent(`Read <worktree>/AGENTS.md first. Worktree: ${REPO}/.worktrees/m2c-fixes (branch m2c/fixes). You own the whole worktree this stage (single lane). Conventions: logical-unit commits via "git -C <worktree>", conventional titles <= 52 chars, NO trailers, explicit staging, no slop comments, never sudo; pre-commit runs make check (~90s incl. go test -race ./...) on the index.

These five interlocking fixes come from the M2b lanes' own concern reports (serve lane especially). Read cmd/exocomp/serve.go, internal/queue/queue.go, cmd/exocomp/workspace.go, cmd/exocomp/playbook.go, internal/backup/engine.go before starting.

1. DAEMON-AWARE CLI (the claim race): when "exocomp serve" is running (the state-backend address file in the data dir + /healthz answering), "workspace plan/apply" and "playbook run" must NOT start their own queue drainer — they enqueue, then TAIL the job's events from the store until a terminal state, and exit with the job's exit code. When no daemon is detected, current inline-drain behavior stays. The serve integration test's accepted-race case (job claimed by serve, CLI reports running with non-zero exit) must be replaced by a deterministic test: daemon up, CLI enqueues, daemon executes, CLI tails to completion and exits with the right code.
2. SERVE MUST RUN ANSIBLE JOBS CORRECTLY: playbook-enqueued jobs carry their vault project in args_json (never the password); serve's kindRouter builds the ansible driver per job (envelope-derived vault password + redactor fed with sealed plaintexts + derived password) exactly as the playbook CLI does — share the construction in one place instead of duplicating (a small helper in cmd/exocomp or internal/executor is fine). Test: with the daemon running, "playbook run" with a vaulted var completes via the DAEMON's claim, succeeds, redaction holds (plaintext in zero job_event rows).
3. FORCED-CANCEL BACKUP FINALIZATION: internal/backup/engine.go runLeg must finish its backup_run row under context.WithoutCancel so a cancelled leg records failed/cancelled instead of staying 'running'. Reproduce first (the serve lane's --drain-grace 0 mutation described exactly how), then fix, then show the row reaches a terminal state.
4. SINGLE-INSTANCE LOCK: two "exocomp serve" processes against one data dir double-fire schedules. Take an exclusive flock on <dataDir>/serve.lock at startup; a second serve exits 1 naming the holder pid. Test with two real processes.
5. SHARED CONSTANT + DOCS: move the "workspace:" runbook prefix literal to one shared location (workspace.go + serve.go both use it); add docs/operations/serve.md (flags --drain-grace/--reload-interval/--listen, the signal contract SIGTERM/SIGHUP, single-instance lock, daemon-aware CLI behavior) + a docs/log.md entry; link from docs/operations/index.md.

Verification with REAL output: make check; go test ./cmd/exocomp/ -race -count=2 (integration tests stable); the deterministic daemon-claims-CLI-job test output for both tofu and ansible paths; the two-serve lock demo; the cancelled-backup terminal-row demo.
Host: go 1.26, tofu, ansible-playbook, restic, age, sops, podman. Never sudo.`,
  { label: 'fix:m2c', phase: 'Fix', model: 'opus', schema: SCHEMA })

return { result }