export const meta = {
  name: 'exocomp-m2a-core',
  description: 'M2a: tofu core, scheduler + backup jobs, hardening lanes',
  phases: [
    { title: 'Build', detail: 'tofu (opus) + scheduler (sonnet) + hardening (sonnet)' },
  ],
}

const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args
const WT = REPO + '/.worktrees'

const COMMON = `
Read <worktree>/AGENTS.md and <worktree>/.agents/workflows/m1-plan.md — its final "M2" section is your stage plan, binding decisions, ownership map and gate. Conventions: work ONLY in your worktree on your lane's paths; logical-unit commits via "git -C <worktree>"; conventional titles <= 52 chars; NO trailers; explicit staging; no slop comments; never sudo. Hooks live: pre-commit runs make check (fmt/vet/golangci-lint/tidy/go test -race ./...) on the index — it takes ~80s, be patient, never bypass it.
Existing building blocks to REUSE (read before writing): internal/secrets (Envelope.Derive, redactor), internal/store (migrations pattern in internal/store/migrate.go + migrations/, typed CRUD style), internal/queue (Enqueue, Option, WithRedactor, EventSink), internal/executor (Driver/Spec/Result, shell + ansible drivers as reference).
Subprocess success is exit-code-only. Secrets never in argv, env dumps, events, or files outside 0700 dirs. Verification with REAL output.
Host: go 1.26, tofu (OpenTofu v1.11.5), ansible-playbook, restic, age, sops, podman, limactl. Never sudo.
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
YOUR LANE: tofu. Worktree: ${WT}/m2a-tofu (branch m2a/tofu). Ownership per the M2a map (internal/api/**, internal/executor/tofu*.go, cmd/exocomp/serve.go + moving the serve stub out of main.go, cmd/exocomp/workspace.go, migration 0003_tofu.sql, internal/store/tofu.go).

Build the Terraform-Cloud half per docs/adr/0004 and docs/architecture/execution.md:
1. Migration 0003: workspace(id, name UNIQUE, tofu_dir, auto_apply, serial), tfstate(id, workspace_id, serial, lineage, created_at, created_by_job_id, ciphertext BLOB), tflock(workspace_id PK, lock_id, who, operation, created_at, job_id). Typed CRUD in internal/store/tofu.go; state append keeps every serial; lock acquire = INSERT (UNIQUE PK -> atomic), release checks lock_id.
2. internal/api: the OpenTofu HTTP state backend — handlers GET (latest ciphertext or 404), POST ?ID= (verify lock holder, serial+1), LOCK (423 + holder JSON on conflict), UNLOCK; plus GET /healthz. Auth: Bearer token per workspace derived base64(Derive("tf-backend", workspace)) — constant-time compare; server binds 127.0.0.1 with a caller-chosen port (0 = ephemeral, report actual). Wire "exocomp serve" (move the stub from main.go into cmd/exocomp/serve.go): opens store+envelope, starts the backend, prints the address, clean shutdown on SIGINT/SIGTERM.
3. internal/executor/tofu.go: driver for Spec kind "tofu": runs tofu init/plan/apply/state via exec with -no-color -input=false; injects a generated backend override file (0700 private dir) pointing at the local server address with the workspace bearer; sets TF_ENCRYPTION (pbkdf2 key provider) with passphrase base64(Derive("tofu-state", workspace)) via env; plan uses -detailed-exitcode (2 = changes -> surfaced in Result, not failure); apply consumes a saved plan file artifact when provided. Allowlisted env like the ansible driver.
4. cmd/exocomp/workspace.go: "workspace add <name> --dir <tofudir>", "workspace list", "workspace plan <name>", "workspace apply <name> [--plan <artifact>]" — plan and apply run through the queue (concurrency_key "tofu:<workspace>"), apply after plan links parent_job via the existing job columns if present (add args_json linkage otherwise), events streamed, exit code passthrough.
5. Tests (-race): store CRUD + lock atomicity (two goroutines, one wins, loser gets holder info); backend handler tests over httptest incl. auth reject + lock conflict 423; REAL end-to-end with the tofu binary: a workspace whose tofu_dir manages local_file resources in a temp dir — init/plan/apply through the queue against a live in-process backend server; assert: state row ciphertext does NOT contain plaintext resource values (grep the BLOB), serial increments, "tofu state list" works via the driver, plan-only run reports changes-present without applying, second concurrent apply on the same workspace serializes via concurrency key AND backend lock, drift: mutate the file on disk, plan exits 2 -> surfaced.
Gate: make check; go test ./internal/api/ ./internal/executor/ ./internal/store/ -race -count=1; the real workspace add/plan/apply CLI sequence output from a scratch XDG_DATA_HOME with serve running.`,
    { label: 'lane:tofu', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: scheduler. Worktree: ${WT}/m2a-sched (branch m2a/sched). Ownership per the M2a map (internal/sched/**, migration 0004_schedule.sql, internal/store/schedule.go, cmd/exocomp/schedule.go, internal/backup/job.go + minimal cmd/exocomp/backup.go wiring). Dep: go get github.com/robfig/cron/v3.

1. Migration 0004: schedule(id, name UNIQUE, cron_expr, kind, args_json, concurrency_key, jitter_s, enabled, last_job_id, next_at). Typed CRUD in internal/store/schedule.go.
2. internal/sched: a Scheduler over (store, queue): loads enabled schedules on Start, registers them with robfig/cron/v3 (seconds field enabled), applies optional jitter sleep, enqueues through the EXISTING queue Enqueue (never a side door), stamps last_job_id/next_at; Reload() rebuilds from the DB; clean Stop(). Schedules of kind "shell" (argv) and "backup" (target name or all).
3. Backup-as-job (closes the M1 carve-out recorded in docs/architecture/overview.md): internal/backup/job.go — a queue-compatible runner: enqueue kind "backup" jobs (concurrency_key "backup:<target>") whose execution calls the existing engine for ONE target; "exocomp backup run" switches to enqueuing one job per enabled target and draining the queue inline (preserve output UX); update docs/architecture/overview.md's carve-out paragraph + a docs/log.md entry (you own this correction).
4. cmd/exocomp/schedule.go: "schedule add <name> --cron '<expr>' --kind shell --argv ... | --kind backup [--target X]", "schedule list", "schedule rm"; plus "exocomp scheduler run" (foreground daemon draining queue + scheduler until signal; this is the M2 stand-in until serve unifies).
5. Tests (-race): cron registration fires a @every 1s shell job through the queue (real time, bounded wait); jitter bounds respected; Reload picks up a newly added schedule; disabled schedules skipped; two scheduled backups of one target serialize on the concurrency key (use a slow fake engine hook or a tiny real target); reboot simulation: new Scheduler from the same DB rebuilds and fires.
Gate: make check; go test ./internal/sched/ ./internal/backup/ -race -count=1; real CLI: schedule add @every 2s shell + scheduler run for ~6s shows >=2 fired jobs; a scheduled backup lands a backup_run row via a queued job.`,
    { label: 'lane:scheduler', phase: 'Build', model: 'sonnet', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: hardening. Worktree: ${WT}/m2a-hard (branch m2a/hard). Ownership per the M2a map (internal/backup/chunk.go, s3 credential resolution in internal/backup/s3.go, serve --ssh via wish in cmd/exocomp/ssh.go). Dep for wish: go get charm.land/wish (or its current canonical path; check what bubbletea v2 pairs with — charm.land/wish v1.x with bubbletea middleware; if the v2-compatible wish is elsewhere, find it and record the path).

1. Git-leg chunking (docs/design/amendments.md item 2): internal/backup/chunk.go — split the age-encrypted archive into 90 MB parts (snapshot-<ts>.age.partNN + a parts manifest with sha256s), reassembly on restore; repo-size guard: refuse the leg loudly when the bare repo (du of objects) exceeds 4 GB, recording the failure in backup_run.error. Wire into the existing git leg + restore path (you own internal/backup/git.go's chunk call sites — coordinate: the file is otherwise stable this stage). Tests: synthetic >90MB (sparse/compressible data is fine) chunks and reassembles byte-identically; size guard triggers on a faked du.
2. Sealed S3 credentials: extend the s3 credential resolution so target env is built from sealed secret rows named "s3/<target>/access-key-id" and "s3/<target>/secret-access-key" (+optional region/session-token) when present, falling back to process env WITH a warning event; never print values. Test with the MinIO helper already in s3_test.go: seal creds, clear env, backup succeeds.
3. SSH TUI: cmd/exocomp/ssh.go — "exocomp ssh-serve --listen 127.0.0.1:2222 --authorized-keys <file>": wish server whose session middleware runs the existing internal/tui model over the SSH pty (bubbletea middleware), public-key auth against the authorized_keys file only (no passwords), host key generated into the data dir 0600 on first run. Test: scripted probe with ssh -o StrictHostKeyChecking=no -i <testkey> sending 'q' — session renders the TUI header and exits cleanly; wrong key rejected. If the wish/bubbletea-v2 pairing is broken upstream, deliver the server with a plain line-mode fallback renderer and record exactly why.
Gate: make check; go test ./internal/backup/ -race -count=1 (incl. new chunk + sealed-creds tests); the real ssh probe transcript.`,
    { label: 'lane:hardening', phase: 'Build', model: 'sonnet', schema: SCHEMA }),
])

return { lanes: results.filter(Boolean) }