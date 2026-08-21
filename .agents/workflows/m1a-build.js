export const meta = {
  name: 'exocomp-m1a-core',
  description: 'M1a: secrets envelope and store/queue lanes in worktrees',
  phases: [
    { title: 'Build', detail: 'secrets (opus) + store/queue (opus)', model: 'opus' },
  ],
}

const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args
const WT = REPO + '/.worktrees'

const COMMON = `
Read <worktree>/AGENTS.md (the contract) and <worktree>/.agents/workflows/m1-plan.md (your stage plan, interface contracts, and gate) FIRST. Repository conventions in brief:
- Work ONLY inside your worktree; commit as you go in logical units via "git -C <worktree>"; conventional titles <= 52 chars; NO trailers of any kind; stage explicit paths, never add -A; no AI-slop comments (comments only for constraints code cannot show); never sudo.
- Hooks are active (core.hooksPath): commit-msg enforces style, pre-commit runs "make check" against the INDEX — golangci-lint IS installed and live, so your code must pass fmt/vet/staticcheck/errcheck/revive/tidy/test to commit at all.
- The interface contracts in m1-plan.md are binding. If an interface must change, keep the change minimal and record why in the commit body.
- Verification before completion: run the gate commands for your lane and report their REAL output.
Host: go 1.26, age CLI v1.3.1 and restic 0.19.1 and sops 3.13.3 on PATH, podman, no sudo.
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
YOUR LANE: secrets. Worktree: ${WT}/m1a-secrets (branch m1a/secrets). You own internal/secrets/** and cmd/exocomp/secrets.go exclusively. Dep: go get filippo.io/age (library) only.

Implement per m1-plan.md "Interface contracts":
1. internal/secrets: the envelope. Init(dataDir, extraRecipients) generates an X25519 host identity (file host.key, 0400, inside dataDir) and a separate recovery identity whose SECRET key appears ONLY in the returned RecoveryKit (never written to disk), generates a 32-byte DEK from crypto/rand, wraps the DEK to each recipient with age (one armored stanza per recipient in a keyring file), and returns Envelope + RecoveryKit. Open(dataDir) loads host identity + keyring and unwraps the DEK. Seal/OpenSecret: AES-256-GCM, random 96-bit nonce per seal, AAD = name+"|"+kind; SealedSecret carries name, kind, nonce, ciphertext. Derive(purpose, id): HKDF-SHA256(secret=DEK, salt="exocomp/v1", info=purpose+":"+id) -> 32 bytes. KeyringAge() returns the keyring bytes (age ciphertext, safe to publish). DEK handling: keep in one []byte, zero it on Close(); do not log it; no fmt of secret material anywhere.
2. NewRedactor(known [][]byte): streaming replacer that scrubs every known byte-pattern (and their base64.StdEncoding and hex encodings) from an io.Writer pipeline, replacing with "[REDACTED]". Must handle patterns split across Write boundaries.
3. cmd/exocomp/secrets.go: registerCommand(...) adding "init" (runs Init against the doctor's data dir, prints the rendered recovery kit text to stdout, refuses if a keyring already exists) and "recovery-kit" (re-renders kit STRUCTURE from stored public data + a freshly provided recovery flow is NOT possible without re-keying, so this subcommand prints keyring recipients + fingerprints + the derivation recipe, and states plainly that the recovery SECRET key is shown only at init or rotation). Kit text includes: the recovery age secret key (init only), recipients fingerprints, salt "exocomp/v1", and the derivation recipe in plain words.
4. Tests (all in-package, must run under -race): round-trip; tamper (flip a ciphertext byte -> open fails; change name -> AAD failure); wrong-identity Open fails; Derive fixed test vectors committed as constants (generate once, assert stable); redactor: property-style test embedding known secrets (raw, base64, hex) at random offsets across chunked writes and asserting zero occurrences in output; cross-tool proof: write KeyringAge() to a temp file and shell out to the age CLI with the recovery identity to decrypt it, asserting the DEK matches (skip with clear message if age absent — it is present on this host, so it must actually run).
Gate commands to run and report: make check; go test ./internal/secrets/ -race -count=1; XDG_DATA_HOME=$(mktemp -d) ./bin/exocomp init (build first) showing the kit; second init refusing.`,
    { label: 'lane:secrets', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: store. Worktree: ${WT}/m1a-store (branch m1a/store). You own internal/store/**, internal/queue/**, internal/executor/driver.go, internal/executor/shell.go, cmd/exocomp/devrun.go exclusively. Dep: go get modernc.org/sqlite only.

Implement per m1-plan.md "Interface contracts":
1. internal/store: Open(path) opens SQLite via modernc.org/sqlite (WAL, busy_timeout=5000, synchronous=NORMAL, foreign_keys=ON) and migrates via PRAGMA user_version with embedded forward-only migrations. Schema (essential columns, from the accepted design): job(id, runbook, kind, trigger, state, queued_at, started_at, finished_at, exit_code, concurrency_key, dedup_key, idempotency_key UNIQUE NULL), job_event(job_id, seq, ts, level, msg, raw_json) append-only, artifact(id, job_id, kind, path, sha256, size), keyring(id, kind, recipient, label, created_at, revoked_at), secret(id, name UNIQUE, kind, nonce, ciphertext, aad, created_at, rotated_at, last_used_at), backup_target(id, name UNIQUE, kind, uri, retention, enabled), backup_run(id, target_id, started_at, finished_at, state, snapshot_id, bytes_added, error). Typed CRUD for each; job state changes ONLY through TransitionJob(id, from, to) which errors on illegal moves per the state machine in m1-plan.md; boot-time sweep: SweepInterrupted() moves queued/running jobs whose process is gone to 'interrupted'.
2. internal/queue: an in-process queue draining job rows: Enqueue(spec) (respecting idempotency_key uniqueness -> returns existing job), a worker loop with per-concurrency_key serialization (two jobs sharing a key never run concurrently; distinct keys may), context cancel + per-job timeout -> state timed_out, events streamed to store.job_event via an EventSink.
3. internal/executor: driver.go defines the Driver interface + Spec + Result + EventSink exactly as in m1-plan.md; shell.go implements the M1 local driver with exec.CommandContext, stdout/stderr line-scanned into events, Result.ExitCode from the process — success is exit-code-only.
4. cmd/exocomp/devrun.go: registerCommand(...) adding a hidden dev command "devrun -- <argv...>" that opens the store at the data dir, enqueues a shell job, runs the queue inline until completion, and prints the final state + last 5 events (this is scaffolding for M1b/M1c verification, marked Hidden: true).
5. Tests (-race required): migration idempotence (Open twice); illegal transition rejected; happy-path job through queue+shell driver (run /bin/true, /bin/false, sleep with timeout); concurrency: N jobs sharing one key execute strictly serially (assert via timestamps or a shared counter file), while two distinct keys overlap; SweepInterrupted turns a fake orphaned 'running' row into 'interrupted'; idempotency_key dedup returns the same job.
Gate commands to run and report: make check; go test ./internal/store/ ./internal/queue/ ./internal/executor/ -race -count=1; ./bin/exocomp devrun -- /bin/echo hello (build first) showing succeeded state.`,
    { label: 'lane:store', phase: 'Build', model: 'opus', schema: SCHEMA }),
])

return { lanes: results.filter(Boolean) }