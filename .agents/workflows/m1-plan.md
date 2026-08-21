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
lanes add their own file, never edit shared files. `go.mod` deps are pre-added by the
integrator before lanes branch; lanes must not run `go get`.

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
