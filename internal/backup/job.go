package backup

import (
	"context"
	"fmt"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// JobKind is the queue.Spec/executor.Spec kind a backup job carries. Backups
// running as queued jobs closes the M1 carve-out recorded in
// docs/architecture/overview.md: they go through the same guardrails as
// every other execution path instead of being a direct CLI invocation.
const JobKind = "backup"

// backupArgv0 is the argv[0] a backup job's spec carries. It is never exec'd
// — Runner intercepts kind JobKind before any shell driver would try — but a
// job row needs a non-empty, legible argv: queue.Enqueue rejects an empty
// one, and it is what "jobs list" would show.
const backupArgv0 = "exocomp-backup"

// Argv builds the argv a queued backup job carries for one target.
func Argv(target string) []string {
	return []string{backupArgv0, target}
}

// ConcurrencyKey is the queue concurrency key backup jobs for one target
// share, so two backups of the same target never run at once.
func ConcurrencyKey(target string) string {
	return "backup:" + target
}

// TargetOf recovers the target name from a backup job's argv, the inverse of
// Argv.
func TargetOf(argv []string) (string, error) {
	if len(argv) != 2 || argv[0] != backupArgv0 {
		return "", fmt.Errorf("backup: not a backup job argv: %q", argv)
	}
	return argv[1], nil
}

// runFunc runs an Engine over a set of targets. It exists so tests can
// substitute a slow or failing stand-in without a real repository; NewRunner
// wires it to (*Engine).Run.
type runFunc func(ctx context.Context, targets []Target) (Report, error)

// Runner executes one target's backup leg as a queue job. It implements
// executor.Driver so kind-JobKind jobs run through the same queue as shell
// and ansible jobs: a backup is Go code driving restic/git subprocesses of
// its own, not itself one subprocess a shell driver could exec, so it needs
// its own driver rather than an argv the shell driver runs.
type Runner struct {
	store     *store.Store
	run       runFunc
	targetEnv func(store.BackupTarget) map[string]string
}

// NewRunner returns a Runner over a store and an open envelope. targetEnv
// resolves the process environment a target's leg needs (S3 credentials, a
// restic cache dir); nil means no target gets extra environment.
func NewRunner(st *store.Store, env *secrets.Envelope, opts Options, targetEnv func(store.BackupTarget) map[string]string) *Runner {
	eng := New(st, env, opts)
	return &Runner{store: st, run: eng.Run, targetEnv: targetEnv}
}

// Run implements executor.Driver. spec.Argv is the job's Argv (see Argv);
// the exit code is 0 when the target's one leg succeeded, 1 otherwise —
// exit-code-only correctness, same as every other driver (AGENTS.md
// invariant 3). The engine writes the target's backup_run row itself, so a
// job succeeding or failing here always has one to show for it.
func (r *Runner) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	started := time.Now()

	name, err := TargetOf(spec.Argv)
	if err != nil {
		return executor.Result{}, err
	}

	row, err := r.store.GetBackupTarget(ctx, name)
	if err != nil {
		return executor.Result{}, fmt.Errorf("backup: target %q: %w", name, err)
	}
	if !row.Enabled {
		return executor.Result{}, fmt.Errorf("backup: target %q is disabled", name)
	}

	var env map[string]string
	if r.targetEnv != nil {
		env = r.targetEnv(row)
	}

	report, err := r.run(ctx, []Target{{BackupTarget: row, Env: env}})
	if err != nil {
		return executor.Result{}, err
	}

	exitCode := 0
	if len(report.Legs) == 0 {
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelError, Msg: "no leg ran"})
		exitCode = 1
	}
	for _, leg := range report.Legs {
		level := executor.LevelInfo
		msg := fmt.Sprintf("target %s: %s snapshot=%s added=%d", leg.Target, leg.State, leg.SnapshotID, leg.BytesAdded)
		if leg.Err != nil {
			level = executor.LevelError
			msg += " error=" + leg.Err.Error()
			exitCode = 1
		}
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: level, Msg: msg})
	}

	return executor.Result{ExitCode: exitCode, Duration: time.Since(started)}, nil
}
