export const meta = {
  name: 'exocomp-m1-fixes',
  description: 'M1 review fixes: security/DR lane + gate/docs lane',
  phases: [
    { title: 'Fix', detail: 'security+DR (opus) · gate+docs (sonnet)' },
  ],
}

const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args
const WT = REPO + '/.worktrees'

const COMMON = `
Read <worktree>/AGENTS.md first. Conventions: work ONLY in your worktree on your lane's files; logical-unit commits via "git -C <worktree>", conventional titles <= 52 chars, NO trailers, explicit staging, no slop comments, never sudo. Hooks live: pre-commit runs make check (incl. golangci-lint) on the index. Verification before completion with REAL output.
These fixes come from an adversarial review; each item below states the finding — fix the cause, not the symptom, and make the fix's test able to fail.
Host: go 1.26, ansible-playbook, restic, age, sops, openssl, podman, limactl; lima instance "exocomp-node0" exists (stopped).
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

phase('Fix')
const results = await parallel([
  () => agent(`${COMMON}
YOUR LANE: security/DR. Worktree: ${WT}/m1fix-a (branch m1fix/security). You own: internal/secrets/kit.go + kit_test.go, internal/backup/restic.go, internal/backup/git.go, internal/backup/manifest.go (+ its docs statement), internal/store/job.go sweep wiring points in cmd/, cmd/exocomp/doctor.go, cmd/exocomp/backup.go (canary removal), packaging/install.sh.

Items:
1. Recovery card (kit.go): step 4 currently references a "read-only storage credential from this card" that the card does NOT carry — reword to the honest current state: the storage location/credential must be provided by the operator (per-target credentials arrive on the card in M2 per docs/design/amendments.md item 1; do not fake it). Step 5 says "exocomp doctor --deep" — that flag does not exist; change to "exocomp doctor". Extend kit_test.go's cardCommands execution so the derived password is proven against a REAL fixture restic repository (restic init with the derived password + restic cat config), closing the docs/log.md claim; restic is on PATH.
2. install.sh: the no-binary restore path currently prints "would restore" and exits 0 — a DR entrypoint reporting success having done nothing. Implement the manual chain in POSIX sh: prompt/flags for target URI + recovery key file, fetch keyring.age from the target root (file path or via restic-less copy for local; document that S3/remote needs the operator to fetch keyring.age first), derive the restic password with age+od+openssl exactly as the card states, run restic restore latest into --dest. Exit non-zero on any failure. Test it for real against a local repo created by the Go engine (build bin/exocomp in the worktree, init+backup in a scratch XDG_DATA_HOME, then install.sh restore WITHOUT the binary on PATH).
3. restic.go: the derived repo password goes into the child environment while the ansible lane deliberately keeps equivalent material out of env — switch to RESTIC_PASSWORD_FILE pointing at a 0600 file in the existing 0700 staging dir, removed with it.
4. git.go: subprocess stderr can echo credential-bearing remote URLs into backup_run.error — scrub URI userinfo from any recorded error text, and make "backup add-target" warn on a --uri containing userinfo.
5. doctor.go: stop creating the data dir as a side effect; stat and report absent-but-creatable vs existing-and-writable, and report (not silently accept) a mode looser than 0700. Verify init creates the dir 0700 (fix if it does not).
6. Boot sweep: SweepInterrupted is only called by devrun and uses pid equality that breaks across container restarts (pid 1 == pid 1). Wire the sweep into every cmd/ path that opens the store for job work (playbook, devrun, tui at least) and replace the ownership test with a liveness check (kill(pid,0) semantics via syscall, plus treating our own pid as live only if the row's started_at is after process start).
7. Delete the hidden "backup canary" command (secrets add supersedes it); update anything that referenced it.
Gate: make check; go test ./internal/secrets/ ./internal/backup/ ./internal/store/ -race -count=1; the real install.sh no-binary restore run's output; the kit_test restic proof output.`,
    { label: 'fix:security-dr', phase: 'Fix', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: gate/docs. Worktree: ${WT}/m1fix-b (branch m1fix/gate). You own: test/e2e/m1-gate.sh, docs/**, AGENTS.md (one row), internal/api/doc.go (one line), Makefile, internal/store/migrations/** + a vacuum test file internal/store/vacuum_test.go, internal/tui/** (two small fixes), internal/queue/queue_test.go (one nit), internal/backup/crypto.go (one comment), cmd/exocomp/tui.go.

Items:
1. test/e2e/m1-gate.sh — make it actually prove the milestone: (a) add a restic_s3 leg backed by MinIO under podman (mirror internal/backup/s3_test.go's startMinIO approach in shell: podman run -d --rm quay.io/minio/minio server /data on a random 127.0.0.1 port, wait for readiness, export the AWS_* env the engine reads; stop the container in the trap); (b) after backup, restore from EACH leg (restic_local, restic_s3, git) into separate dests; (c) replace the secrets-list canary check with "exocomp restore ... --canary canary" so the canary actually decrypts (flag exists in cmd/exocomp/restore.go); (d) make the redaction assertion non-vacuous: add an ansible.builtin.debug task printing the vaulted var, then assert BOTH that the plaintext is absent from exocomp.db AND that "[REDACTED]" appears in the job's captured output/events; (e) update header comment + PASS line to say three legs truthfully. RUN THE GATE FOR REAL and report its final line (10 min allowance; lima instance exists stopped).
2. docs corrections, each with a docs/log.md entry: (a) five doc rows say the executor is ansible-runner with per-task JSON events — record the M1 substitution (direct ansible-playbook driver, line-scanned stdout, exit-code correctness) and state ansible-runner is re-evaluated in M2; (b) operations/disaster-recovery.md legs table + failure-mode table: mark git-leg chunking/size-guard as M2 and align every command with the shipped flag surface; (c) architecture/overview.md: the "every execution path goes through the one queue" prose vs the backup side-door — record the explicit M1 carve-out (backup/restore run outside the queue; they become queued jobs with the M2 scheduler) in BOTH overview.md and docs/log.md, so the invariant exemption is documented rather than silent; (d) AGENTS.md api/ row + internal/api/doc.go: api/ lands M2 not M1.
3. Makefile: check gains a race pass (test target with -race, or a test-race target wired into check — keep check runtime sane by using -race only on ./internal/... if full is too slow); add tidy-check to .PHONY.
4. Migration 0002: BEFORE DELETE trigger on job_event completing the append-only claim (forward-only migration; test that DELETE is rejected and that 0001->0002 upgrade works on an existing db).
5. internal/store/vacuum_test.go: test VacuumInto refuses an existing destination and succeeds while a writer is mid-transaction; note the single-connection stall in the vacuum.go comment.
6. TUI: dim or mark disabled targets (align column meaning with backup list), and give cmd/exocomp/tui.go a clean non-TTY error message instead of the raw library error.
7. Nits: fix internal/backup/crypto.go's comment pointing at a non-existent test file (point at the real pinning test); drop the "_ = second" in queue_test.go by using the variable.
Gate: make check (now with race); go test ./internal/store/ ./internal/tui/ -race -count=1; the REAL m1-gate.sh final line showing three legs + [REDACTED] assertion.`,
    { label: 'fix:gate-docs', phase: 'Fix', model: 'sonnet', schema: SCHEMA }),
])

return { lanes: results.filter(Boolean) }