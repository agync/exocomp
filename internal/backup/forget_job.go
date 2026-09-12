package backup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// forgetArgv0 is the argv[0] a forget job's spec carries. Like backupArgv0, it
// is never exec'd — only a legible, non-empty job row and the routing tag
// ForgetTargetOf reads back. It must equal the argv[0] the web verb enqueues
// (internal/web/backups_row_actions.go:106), or a web-enqueued forget job
// would be rejected as malformed here.
const forgetArgv0 = "exocomp-forget"

// ForgetArgv builds the argv a queued forget job carries for one target and
// one full snapshot id — the same shape the web verb enqueues.
func ForgetArgv(target, snapshot string) []string {
	return []string{forgetArgv0, target, snapshot}
}

// ForgetTargetOf recovers the target name and snapshot id from a forget job's
// argv, the inverse of ForgetArgv.
func ForgetTargetOf(argv []string) (target, snapshot string, err error) {
	if len(argv) != 3 || argv[0] != forgetArgv0 {
		return "", "", fmt.Errorf("backup: not a forget job argv: %q", argv)
	}
	return argv[1], argv[2], nil
}

// forgetGuard is the runner-side mirror of the web verb's snapshot gate
// (internal/web/backups_data.go): a forget may name only a snapshot that is a
// succeeded run of the target, and never the newest one. The newest is the
// recovery point — and a drilled snapshot is the newest at drill time, so one
// check refuses both. An id that is no succeeded run of the target is refused
// the same way the web page refuses one: a stale or crafted id never reaches
// restic.
func (r *Runner) forgetGuard(ctx context.Context, t store.BackupTarget, snapshot string) error {
	runs, err := r.store.ListBackupRuns(ctx, t.ID, 0)
	if err != nil {
		return err
	}
	newest := ""
	known := false
	for _, run := range runs {
		if run.State != store.BackupSucceeded || run.SnapshotID == "" {
			continue
		}
		if newest == "" {
			newest = run.SnapshotID
		}
		if run.SnapshotID == snapshot {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("backup: forget: snapshot %s is not on target %q's history", snapshot, t.Name)
	}
	if snapshot == newest {
		return fmt.Errorf("backup: forget: snapshot %s is the newest snapshot of %q — the recovery point", snapshot, t.Name)
	}
	return nil
}

// runForget implements the ForgetJobKind leg of Runner.Run: one target's
// per-snapshot forget, fired by the web verb (SPEC §7 A12). Like a drill it
// writes no backup_run row — it is not a backup — so its only trace is the
// job's own events; a refused forget and a restic forget that exits non-zero
// both settle the job failed with the exit code carrying the truth (invariant
// 3). The verb names one snapshot, so the command is a plain
// `restic forget <id> --prune` — no retention flags; the target's retention
// policy is re-applied on the next backup's prune. A nil driver falls back to
// the local shell driver, the same default the drill and the engine use.
func (r *Runner) runForget(ctx context.Context, spec executor.Spec, sink executor.EventSink) (res executor.Result, err error) {
	started := time.Now()

	var red *secrets.Redactor
	defer func() { err = scrubbed(redacted(red, err)) }()

	name, snapshot, err := ForgetTargetOf(spec.Argv)
	if err != nil {
		return res, err
	}

	row, err := r.store.GetBackupTarget(ctx, name)
	if err != nil {
		return res, fmt.Errorf("backup: forget: target %q: %w", name, err)
	}
	if !row.Enabled {
		return res, fmt.Errorf("backup: forget: target %q is disabled", name)
	}
	if err := r.forgetGuard(ctx, row, snapshot); err != nil {
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelError,
			Msg: "target " + name + ": forget refused: " + err.Error()})
		return executor.Result{ExitCode: 1, Duration: time.Since(started)}, nil
	}

	target := Target{BackupTarget: row}
	if r.targetEnv != nil {
		target.Env = r.targetEnv(row)
	}

	key := r.env.Derive("restic-repo", name)
	if len(key) != dekLen {
		return res, errors.New("backup: forget: envelope is closed")
	}
	password := base64.StdEncoding.EncodeToString(key)

	repo, err := resticRepo(target)
	if err != nil {
		return res, fmt.Errorf("backup: forget: %w", err)
	}
	if target.Kind == KindResticS3 {
		resolved, _, err := resolveS3Env(ctx, r.store, r.env, target.Name, target.Env)
		if err != nil {
			return res, fmt.Errorf("backup: forget: %w", err)
		}
		target.Env = resolved
	}

	passFile, cleanup, err := writePasswordFile(r.stageRoot, password)
	if err != nil {
		return res, err
	}
	defer cleanup()

	red = secrets.NewRedactor(legSecrets(password, target.Env))

	driver := r.driver
	if driver == nil {
		driver = executor.NewShell()
	}

	out, err := runTool(ctx, driver, executor.Spec{
		Kind: "shell",
		Argv: []string{"restic", "-r", repo, "forget", snapshot, "--prune"},
		Dir:  r.stageRoot,
		Env:  resticEnv(target.Env, passFile),
	}, red)
	if err != nil {
		return res, fmt.Errorf("backup: forget: %w", err)
	}

	if out.exitCode != 0 {
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelError,
			Msg: fmt.Sprintf("target %s: forget snapshot=%s failed: %s", name, snapshot, out.tail(5))})
		return executor.Result{ExitCode: 1, Duration: time.Since(started)}, nil
	}

	sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelInfo,
		Msg: fmt.Sprintf("target %s: forgot snapshot=%s", name, snapshot)})
	return executor.Result{ExitCode: 0, Duration: time.Since(started)}, nil
}
