export const meta = {
  name: 'exocomp-m1b-surfaces',
  description: 'M1b: ansible executor, backup/restore engine, TUI lanes',
  phases: [
    { title: 'Build', detail: 'ansible (opus) + backup (opus) + tui (sonnet)' },
  ],
}

const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // pass {repo: ...} as workflow args
const WT = REPO + '/.worktrees'

const COMMON = `
Read <worktree>/AGENTS.md and <worktree>/.agents/workflows/m1-plan.md (M1b ownership map + gate) FIRST. Conventions in brief:
- Work ONLY in your worktree and ONLY on paths your lane owns per the M1b map; commit logical units via "git -C <worktree>"; conventional titles <= 52 chars; NO trailers; explicit staging only; no AI-slop comments; never sudo.
- Hooks are live: pre-commit runs "make check" on the INDEX with golangci-lint; your code must pass fmt/vet/lint/tidy/test to commit.
- Existing code on your branch: internal/secrets (envelope: Init/Open/Seal/OpenSecret/Derive/KeyringAge, NewRedactor with Wrap(io.Writer)), internal/store (typed CRUD, TransitionJob, migrations), internal/queue (Enqueue, worker loop, EventSink), internal/executor (Driver/Spec/Result/EventSink, shell driver). Read them before writing; reuse, don't duplicate.
- Subprocess success is exit-code-only. Secrets never in logs, job events, or committed files.
- Verification before completion: run your gate commands, report REAL output.
Host: go 1.26, golangci-lint, ansible-playbook (uv-installed), restic 0.19.1, age 1.3.1, sops 3.13.3, podman 5.8.4, git. Never sudo.
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
YOUR LANE: ansible. Worktree: ${WT}/m1b-ansible (branch m1b/ansible).

Deliverables:
1. internal/executor/ansible.go — AnsibleDriver implementing executor.Driver for Spec kind "ansible": builds an ansible-playbook invocation (playbook path, -i inventory path or inline "localhost," with ansible_connection=local, extra vars file); environment is an explicit allowlist (PATH, HOME, LANG, ANSIBLE_*) — never the parent env wholesale; a per-job private dir via os.MkdirTemp with 0700 holding any vault password file, removed (best-effort with defer) after the run; stdout/stderr streamed to events line by line; Result.ExitCode from the process, exit-code-only correctness. Vault: when Spec carries a vault password source (from envelope Derive("ansible-vault", project) or a sealed secret), write it to the private dir and pass --vault-id label@file.
2. internal/queue/redact.go — a queue Option WithRedactor(*secrets.Redactor) that wraps the queue's EventSink so every event line is scrubbed before persistence; wire the option point into queue construction (minimal touch of the existing queue file is allowed for the option hook only).
3. cmd/exocomp/playbook.go — registerCommand: "playbook run <path> [--inventory <path>] [--vault-project <name>]" that opens store+envelope (envelope optional: if not initialized, run without vault and without redactor but WARN), enqueues an ansible job, runs it inline like devrun, streams events to stdout, exits with the job's exit code.
4. Tests (-race): unit tests for argv/env construction (no wholesale env leak — assert an injected canary env var does NOT reach the child); an end-to-end test (guarded by ansible-playbook presence — it IS on this host so it must run): create a temp playbook using debug + a vaulted variable (encrypt with ansible-vault CLI via a password file, or with the envelope-derived password), run through queue+driver with the redactor wired, assert: job succeeded, the plaintext secret value appears in ZERO job_event rows, the vault password file is gone after the run, exit code respected for a failing playbook (a task with failed_when: true -> job failed).
Gate: make check; go test ./internal/executor/ ./internal/queue/ -race -count=1; ./bin/exocomp playbook run <temp playbook> against localhost showing succeeded; the redaction assertion output.`,
    { label: 'lane:ansible', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: backup. Worktree: ${WT}/m1b-backup (branch m1b/backup).

Deliverables:
1. internal/store/vacuum.go (new file only): (*Store).VacuumInto(ctx, path) using VACUUM INTO for a consistent snapshot.
2. internal/backup — the engine per docs/operations/disaster-recovery.md: Engine over (Store, Envelope). Run(ctx, targets): assemble a staging dir (0700 tmp) containing snapshot.db (VacuumInto), keyring.age (Envelope.KeyringAge()), MANIFEST.json (schema version, tool versions, timestamp, sha256 of every file); then fan out per enabled target, INDEPENDENTLY (one leg failing must not abort others; each leg gets a backup_run row with state/error):
   - kind restic_local: restic -r <uri> with RESTIC_PASSWORD = base64(Derive("restic-repo", target.name)); init repo if absent; backup the staging dir; also write keyring.age + RECOVERY.md at the target ROOT (outside the restic repo).
   - kind restic_s3: same via restic s3: backend; S3 credentials from env vars the caller passes per target (document names AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY sourced from sealed secrets later; for now accept a map in the target config).
   - kind git: tar the staging dir, zstd if available else gzip, age-encrypt to ALL keyring recipients, commit as snapshot-<ts>.age to the bare repo at uri via a temp clone, force-push a single orphan commit to refs/heads/backup (the rolling-orphan scheme from docs/design/amendments.md) plus keyring.age committed unencrypted alongside.
   Retention for restic legs: forget --keep-daily 7 --keep-weekly 4 --prune (config per target).
3. Restore: RestoreLatest(ctx, targetKindAndURI, destDataDir) — from restic (restore latest) or from the git leg (clone, take newest snapshot-*.age, decrypt with an identity, untar); verifies MANIFEST sha256s; refuses a dest that already has a keyring.
4. cmd/exocomp/backup.go — "backup run" (all enabled targets from store; targets addable via "backup add-target --kind --name --uri"), "backup list". cmd/exocomp/restore.go — "restore --from <uri-or-kind:uri> --dest <dir> [--identity <agekeyfile>]".
5. packaging/install.sh: make the restore mode call "exocomp restore" when an exocomp binary is present, keeping the current print-what-it-would-do as fallback.
6. Tests (-race where applicable): engine staging correctness (manifest hashes verify); REAL restic round-trip against a t.TempDir local repo (restic is installed — must run, not skip); git leg round-trip against a local bare repo incl. orphan-ref force-push behavior (two runs -> one commit on refs/heads/backup); restore into scratch dir + canary: seal a secret pre-backup, restore, Open envelope from restored dir with the HOST identity file copied? NO — prove the real chain: use the RECOVERY identity output by a test Init, decrypt restored keyring.age with age CLI, derive, open the restored store, read the canary sealed secret and decrypt it; independence: one target with an unreachable uri fails its backup_run while others succeed. S3 leg: if podman can run quay.io/minio/minio quickly, test against it (bind 127.0.0.1, random port, MINIO_ROOT_USER/PASSWORD); if the pull/startup fails, mark the S3 test skipped with the reason in your report — do not fabricate.
Gate: make check; go test ./internal/backup/ ./internal/store/ -race -count=1; a real ./bin/exocomp init + backup run + restore + canary sequence in a scratch XDG_DATA_HOME, output shown.`,
    { label: 'lane:backup', phase: 'Build', model: 'opus', schema: SCHEMA }),

  () => agent(`${COMMON}
YOUR LANE: tui. Worktree: ${WT}/m1b-tui (branch m1b/tui). Deps allowed: github.com/charmbracelet/bubbletea/v2, bubbles/v2, lipgloss/v2 (go get them yourself; if v2 modules are unavailable use v1 paths and say so).

Deliverables:
1. internal/tui — a Bubble Tea program with two screens and a bottom help bar, switchable with [1]/[2] and tab:
   - Jobs: table of recent jobs from internal/store (id, kind, state, queued/finished times, exit code), auto-refresh every 2s via a tick, enter opens a scrollback viewport tailing that job's job_event rows (poll-append), esc back, q quits.
   - Backups: table of backup targets with their latest backup_run (state, snapshot id, bytes, error), refresh on tick.
   Style with lipgloss: restrained monochrome + one accent; state rendered as colored text (succeeded green, failed/timed_out red, running accent, else dim). No emoji.
2. cmd/exocomp/tui.go — registerCommand "tui": opens the store read-only path (same dataDir), errors cleanly if no database exists yet ("run exocomp init / devrun first").
3. Tests: model unit tests driving Update with synthetic messages (screen switching, selection movement, tail toggling) and a store-backed test seeding jobs + events into a temp store and asserting the rendered View contains the expected rows/states. Avoid golden-file brittleness; assert substrings.
Gate: make check; go test ./internal/tui/ -race -count=1; ./bin/exocomp tui exits cleanly with the no-database message on an empty XDG_DATA_HOME (non-TTY: it should print the error, not hang); with a seeded temp store, a scripted tea test (or the store-backed View test) shows both screens render.`,
    { label: 'lane:tui', phase: 'Build', model: 'sonnet', schema: SCHEMA }),
])

return { lanes: results.filter(Boolean) }