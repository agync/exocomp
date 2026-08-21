# M1 — provable core

Goal (docs/design/delivery.md): run Ansible playbooks with encrypted secrets, back up
to three places, prove the restore. Executed as three staged workflows; parallel work
in worktrees with the file-ownership map below; integrator reviews and fast-forwards.

## Stages

- **M1a core**: `internal/secrets` (envelope) and `internal/store` + `internal/queue`
  (persistence, job state machine, executor driver interface).
- **M1b surfaces**: ansible executor, backup/restore engine + real `install.sh` logic,
  two-screen TUI. Depends on M1a interfaces.
- **M1c gate**: end-to-end on a lima node — playbook with an encrypted secret runs;
  backup fans out to local restic + S3-compatible (MinIO via podman) + age-encrypted
  git bundle; restore into a scratch data dir; canary secret round-trips; recovery-kit
  text executes as written. Then the maintenance workflow.

## Decisions binding for M1

- HKDF domain-separation salt: `exocomp/v1` (fresh project, no legacy data; the
  pre-rename archive's `control/v1` is history). Printed on the recovery card.
- Secrets at rest: AES-256-GCM under a 32-byte DEK; `name|kind` as AAD; DEK wrapped
  per age recipient in the keyring (ADR 0003).
- Job states: `queued → running → succeeded|failed|cancelled|timed_out|interrupted`,
  plus `awaiting_approval` reserved (M3). Transitions enforced in one place.
- Subprocess success is exit-code-only (AGENTS.md invariant 3).
- One SQLite file via `modernc.org/sqlite`, WAL, `PRAGMA user_version` migrations,
  forward-only.

## File ownership (M1a)

| Lane | Owns exclusively |
|---|---|
| secrets | `internal/secrets/**`, `cmd/exocomp/secrets.go` (init + recovery-kit cmds) |
| store | `internal/store/**`, `internal/queue/**`, `internal/executor/driver.go` + `internal/executor/shell.go`, `cmd/exocomp/devrun.go` |

`cmd/exocomp/main.go` registers subcommands via the per-file `registerCommand` pattern —
lanes add their own file, never edit shared files. Each lane runs `go get` only for
its own deps (secrets: `filippo.io/age`; store: `modernc.org/sqlite`); the integrator
unions `go.mod`/`go.sum` with `go mod tidy` at merge — lane conflicts there are
expected and mechanical.

## Interface contracts (M1a)

```go
// internal/secrets
func Init(dataDir string, extraRecipients []string) (*Envelope, *RecoveryKit, error)
func Open(dataDir string) (*Envelope, error)            // host identity from dataDir
type Envelope struct{ /* unexported */ }
func (e *Envelope) Seal(name, kind string, pt []byte) (SealedSecret, error)
func (e *Envelope) OpenSecret(s SealedSecret) ([]byte, error)
func (e *Envelope) Derive(purpose, id string) []byte    // HKDF-SHA256, salt exocomp/v1
func (e *Envelope) KeyringAge() ([]byte, error)         // keyring.age bytes, safe ciphertext
func NewRedactor(known [][]byte) *Redactor              // scrubs known secrets from streams
type RecoveryKit struct{ AgeSecretKey, Salt, Derivation string /* renderable */ }

// internal/store
func Open(path string) (*Store, error)                  // migrates to current user_version
// typed CRUD for Job, JobEvent, Artifact, KeyringRow, SecretRow, BackupTarget, BackupRun
// Job transitions only via (*Store).TransitionJob(id, from, to) — illegal moves error.

// internal/executor
type Driver interface {
    Run(ctx context.Context, spec Spec, sink EventSink) (Result, error)
}
// shell.go: the M1 local driver — exec.CommandContext, exit code decides Result.
```

## Gate (M1a)

- `make check` green (lint is live now).
- Envelope: round-trip test; tamper detection (AAD mismatch fails); wrong-identity
  open fails; `Derive` snapshot test (fixed vectors committed); redactor property
  test over random embeddings of known secrets in noisy streams.
- Cross-tool proof: `KeyringAge()` output decrypts with the standalone `age` CLI using
  the recovery identity; test asserts this by shelling out to `age`.
- Store: migration idempotence; illegal transition rejected; concurrent enqueue with
  one `concurrency_key` serializes (race test with `-race`).
- `exocomp init` on a scratch `XDG_DATA_HOME` creates host identity + keyring +
  prints the kit; `exocomp recovery-kit` re-renders it; second `init` refuses.

## File ownership (M1b)

| Lane | Owns exclusively |
|---|---|
| ansible | `internal/executor/ansible*.go`, `internal/queue/redact.go` (+ the one queue option wiring redaction), `cmd/exocomp/playbook.go` |
| backup | `internal/backup/**`, `internal/store/vacuum.go` (new file only), `cmd/exocomp/backup.go`, `cmd/exocomp/restore.go`, `packaging/install.sh` (restore wiring) |
| tui | `internal/tui/**`, `cmd/exocomp/tui.go` (deps: charmbracelet bubbletea/v2, bubbles/v2, lipgloss/v2) |

## Gate (M1b)

- ansible: a real playbook with a vaulted variable runs against localhost through the
  queue; the secret value appears in NO `job_event` row (redactor wired); exit-code-only
  correctness; vault password delivered via a 0700 tmpdir file that is removed after.
- backup: staging tree (vacuumed DB + keyring.age + config) fans out to a local restic
  repo, an S3 restic repo (MinIO via podman), and an age-encrypted archive committed to
  a bare git repo; `keyring.age` lands at every target root outside the restic repo;
  `exocomp restore` rebuilds a scratch data dir from any leg; a canary sealed secret
  round-trips after restore; each leg fails independently.
- tui: `exocomp tui` renders Jobs (live list + event tail) and Backups screens against
  a real store; model unit tests; clean exit.

---

# M2 — both halves

Goal (docs/design/delivery.md): the Terraform-Cloud half joins the AWX half. Staged:
M2a (tofu core + scheduler + queue carve-out closure), M2b (TUI expansion + offline
caches + hardening), M2c (gate + maintenance).

## Decisions binding for M2

- The HTTP state backend lives in `internal/api`, served by `exocomp serve` on
  127.0.0.1 only (unix socket for everything else arrives with the full API); per-job
  bearer tokens derived per workspace, valid for the job's lifetime.
- State is encrypted by tofu itself: `TF_ENCRYPTION` pbkdf2 passphrase =
  base64(Derive("tofu-state", workspace)). Ciphertext rows in `tfstate`, lock in
  `tflock` (UNIQUE PK), version history kept.
- Migration numbers: 0003 tofu tables (workspace, tfstate, tflock) — tofu lane;
  0004 schedule table — scheduler lane. Forward-only as always.
- Backups become queued jobs (kind `backup`, concurrency_key `backup:<target>`),
  closing the M1 carve-out recorded in architecture/overview.md.
- Scheduler: robfig/cron/v3, schedules in the DB, rebuilt on boot; jitter supported.

## File ownership (M2a)

| Lane | Owns exclusively |
|---|---|
| tofu | `internal/api/**` (state backend + serve wiring), `internal/executor/tofu*.go`, `cmd/exocomp/serve.go` (replaces the stub in main.go — move serve out of main.go first, coordinate via this file being yours), `cmd/exocomp/workspace.go`, migration `0003_tofu.sql` + `internal/store/tofu.go` (new files) |
| scheduler | `internal/sched/**`, migration `0004_schedule.sql` + `internal/store/schedule.go` (new files), `cmd/exocomp/schedule.go`, backup-as-job: `internal/backup/job.go` (new; queue-enqueued legs) + minimal wiring in `cmd/exocomp/backup.go` |
| hardening | `internal/backup/chunk.go` (git-leg 90 MB chunking + repo-size guard), sealed S3 credentials (`secrets add` rows named `s3/<target>/access-key` etc. read by the engine; extend `internal/backup/s3.go` credential resolution only), `serve --ssh` via charmbracelet/wish wrapping the TUI, `cmd/exocomp/ssh.go` |

## Gate (M2a)

- tofu: `exocomp workspace add` + a real `tofu plan`/`apply` of a local_file resource
  through the queue, state stored encrypted in sqlite via the HTTP backend (verify:
  the tfstate row is ciphertext, `tofu state list` works via the CLI, plan→apply are
  linked jobs); drift: mutate the file, scheduled plan detects exit-code 2 → job row
  `drift=true` analog; lock contention: second apply blocks/fails cleanly.
- scheduler: a `@every 5s` schedule fires a job through the queue; reboot rebuilds
  schedules from the DB; a scheduled backup runs as a queued job with its
  concurrency key (two backups of one target serialize).
- hardening: git leg chunks a >90 MB synthetic snapshot and refuses at the size
  guard; S3 creds resolve from sealed rows (env fallback warns); `ssh -p <port>
  localhost` renders the TUI over wish (scripted probe).
