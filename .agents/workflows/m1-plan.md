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

## File ownership (M2b)

| Lane | Owns exclusively |
|---|---|
| serve-unify | `cmd/exocomp/serve.go` (daemon = backend + scheduler + queue workers + boot sweep; graceful shutdown finishes in-flight legs via context.WithoutCancel), `cmd/exocomp/schedule.go` (enable/disable subcommands), deprecating `scheduler run` |
| tui-expand | `internal/tui/**` (Workspaces + Schedules screens), `cmd/exocomp/tui.go` |
| caches | tofu driver TF_PLUGIN_CACHE_DIR under the data dir (`internal/executor/tofu.go` cache lines only), `cmd/exocomp/vendor.go` (`playbook vendor` via ansible-galaxy into the data dir) + ANSIBLE_COLLECTIONS_PATH in the ansible driver env (`internal/executor/ansible.go` env lines only), SeaweedFS fixture swap in `internal/backup/s3_test.go` + `test/e2e/m1-gate.sh`, operator docs for `s3/<target>/…` sealed-cred naming |

## Gate (M2b)

- serve: one daemon serves the state backend while a schedule fires and a workspace
  apply runs concurrently; SIGTERM mid-backup leaves no zombie running rows (finish
  writes complete); boot sweep runs at serve start.
- tui: Workspaces (serial, last plan/apply, drift marker) and Schedules (cron,
  next/last) render against a seeded store; four screens navigable.
- caches: after `playbook vendor` and one cached `tofu init`, a play and a plan both
  succeed with network access denied (unshare -rn if available, else galaxy/registry
  endpoints pointed at a black-hole address) — the §3.8 offline-DR property.
- fixture: S3 tests and the e2e gate run against SeaweedFS (Apache-2.0); if any restic
  or SigV4 incompatibility appears, keep the pinned MinIO image and record why.

---

# M3 — v1.0

Stages: M3a (reactive core + sandbox/image + security folds), M3b (web UI + metrics
+ onboarding/wizard + LAN tokens per ADR 0009), M3c (upgrade/rollback + plan policy
+ step ledger per ADR 0008), M3d (v1.0 gate + maintenance). Binding inputs:
docs/design/ui-brief.md "Approval surface contract", docs/design/tui-brief.md,
ADR 0008/0009, internal/web/tokens.css, docs/design/user-feedback.md M3 folds.

## File ownership (M3a)

| Lane | Owns exclusively |
|---|---|
| reactive | internal/reactive/**, internal/notify/**, migration 0005 (event, rule, silence, approval, rule_fire), internal/queue/gate.go (+ minimal queue wiring for the approval gate), internal/sched tofu-plan schedule kind, cmd/exocomp/{rule.go,silence.go,approval.go}, cmd/exocomp/serve.go (ingress + engine wiring) |
| sandbox | internal/executor/sandbox*.go (nono local-sandboxed tier + selection + doctor probes), packaging/Containerfile (uv toolchain layer per ADR 0006), packaging budget make targets + tests |
| secfolds | internal/secrets (keyring version counter, kit derive/verify support), internal/backup (manifest + parts HMAC, downgrade refusal, plan-artifact encryption call sites), cmd/exocomp/kit.go (kit derive [--list] / kit verify / identity migrate), DCO file + AGENTS.md contributor line |

## Gate (M3a)

- reactive: alertmanager-shaped webhook → normalized event → CEL rule → guardrails
  (cooldown ledger, cap disables, dedup, kill switch `pause`) → approval (HMAC,
  TTL fail-closed, stale-plan refusal, resolve-cancels) → ntfy action callback
  (token-authed, simulated in tests) → job through the queue; silences (scoped +
  global, mandatory expiry) swallow with counts; every event persisted with
  outcome + reason; explain trace matches the events design.
- sandbox: a job under local-sandboxed cannot read exocomp.db/host.key/serve.lock
  (probe-asserted); doctor reports tier + Landlock ABI, degrades loudly; image
  builds with the uv toolchain layer self-contained (ansible runs offline from
  the image) and stays ≤800MB with budgets printed by make.
- secfolds: restore refuses a downgraded keyring version and a tampered manifest
  (HMAC); `exocomp kit derive restic/<t>` = engine's password (cross-checked);
  `kit verify` validates a printed page's sha; plan artifacts unreadable at rest.

---

# M3b — v1.0 surfaces + the real source/credential model

REVISED after ADR 0010 and the two expert reviews (docs/design/expert-reviews.md).
The credential-plane binding is now the spine of this milestone, not a side note.
Binding inputs: ADR 0009 (LAN tokens), ADR 0010 (source model + 3 localities),
expert-reviews.md v1 must-fixes, docs/design/{ui-brief,tui-brief}.md + the v0.2
handoff fixtures in internal/tui/testdata, internal/web/tokens.css.

## File ownership (M3b)

| Lane | Owns exclusively |
|---|---|
| sources | internal/source/** (git repo register + auto-sync + deploy key; host-path + in-container-path localities per ADR 0010), migration 0006 (source, inventory, run_secret bindings), cmd/exocomp/{source.go,host.go}, the RUN-ENV CREDENTIAL BINDING in internal/executor/{ansible,tofu}.go (sealed-secret set → named env; the convergent must-fix), ansible flag passthrough (--check/--limit/--tags/-e) + ansible.cfg Dir fix + multi-vault-id, tofu arg passthrough (-var-file/-target/-replace) + workspace add --state-only emit + the workspace-collision doc + offline lock-hash fix (tofu providers lock -fs-mirror) + tfstate pruning policy |
| web | internal/web/** (templ+htmx on tokens.css: dashboard incl. banner states, jobs+tail SSE, phone approval 390px per the approval-surface contract, events+trace), ADR 0009 LAN auth (token new/list/revoke, session cookie, self-signed TLS + fingerprint), cmd/exocomp/{web.go,token.go} |
| ops-ui | internal/tui/** (Home[0]/Approvals[5]/Events[6] screens + banner system + q-root-only + [0]-home, matching internal/tui/testdata/frames golden fixtures; notification-mute for the PAUSED-vs-MUTED banner split), internal/metrics/** (/metrics staleness gauges + seeded alert rules), cmd/exocomp/setup.go (the 4-step wizard: identity→kit→backup+drill→handover, nothing-written-until-step-4), doctor self-observability lines |

## Gate (M3b)

- sources: register a git repo with a generated deploy key, auto-sync a signed
  ref; run a playbook FROM the synced checkout with a DYNAMIC inventory whose
  plugin authenticates from a bound sealed secret (the must-fix — prove the token
  reaches the plugin under the sandbox); the repo's ./ansible.cfg loads; two
  vault-ids resolve; a host-path source and an in-container-path source both run.
  tofu: workspace add --state-only emits a backend block an UNMODIFIED external
  tofu uses successfully (state lands encrypted+locked); mode B runs a provider
  that needs a bound token; -var-file passes; offline init succeeds against the
  fs-mirror lock. All under the sandbox tier.
- web: token-gated LAN listener (ADR 0009 gate assertions: tokenless bind refused,
  revoked token rejected, session dies with token, insecure warns); dashboard +
  phone approval render on tokens.css; approve-from-phone runs a gated job; a11y
  hard reqs (keyboard, focus ring, both themes, 48px).
- ops-ui: the 3 new TUI screens diff clean against the golden fixtures; /metrics
  exposes the staleness gauges + a scrape shows them; the setup wizard runs
  end-to-end writing nothing until step 4; notification-mute yields the MUTED
  banner (jobs still run) distinct from PAUSED.

## M3a residuals carried to v1.1 (from the maintenance fixes)

- Git-snapshot forgery: the AUTHENTICATED.mac marker travels in the repo, so an
  attacker stripping BOTH tag and marker falls back to legacy warn-and-proceed.
  The realistic add-a-forged-snapshot case (marker intact) is refused today;
  fully closing the deletion gap needs an out-of-band marker record (the recovery
  kit already carries per-target facts — add the authenticates-flag there).
- m1-gate depends on lima's ssh ControlPersist master (a pathname AF_UNIX socket
  the sandbox permits); if a future lima drops it the play needs the identity key
  + target port granted. Not a product issue — a test-harness coupling to note.
