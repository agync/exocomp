package backup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// DrillJobKind is the queue.Spec/executor.Spec kind a scheduled restore drill
// carries. It is its own kind rather than JobKind so `jobs list` and the
// job-failure sweep (internal/reactive) tell a drill's outcome apart from an
// actual backup's without parsing argv; internal/sched routes it to the same
// Runner a backup schedule uses (see internal/sched/driver.go).
const DrillJobKind = "backup-drill"

// drillArgv0 is the argv[0] a drill job's spec carries. Like backupArgv0, it
// is never exec'd — only a legible, non-empty job row and the routing tag
// DrillTargetOf reads back.
const drillArgv0 = "exocomp-drill"

// DrillArgv builds the argv a queued restore-drill job carries for one
// target.
func DrillArgv(target string) []string {
	return []string{drillArgv0, target}
}

// DrillConcurrencyKey is the queue concurrency key restore-drill jobs for one
// target share, so two drills of the same target never run at once.
func DrillConcurrencyKey(target string) string {
	return "drill:" + target
}

// DrillTargetOf recovers the target name from a drill job's argv, the
// inverse of DrillArgv.
func DrillTargetOf(argv []string) (string, error) {
	if len(argv) != 2 || argv[0] != drillArgv0 {
		return "", fmt.Errorf("backup: not a drill job argv: %q", argv)
	}
	return argv[1], nil
}

// LiveSource builds a restore Source for a stored target using the running
// envelope's own derivation — the same HKDF resticPassword() writes a backup
// with — so a drill against the live system needs no offline recovery
// identity. This is what makes an unattended, scheduled drill possible: a
// disaster-recovery restore's age-identity chain (restore.go) is for when the
// envelope itself is gone, which is not the case here.
//
// It refuses a git-kind target: a git leg is age-encrypted only to public
// recipients (docs/operations/disaster-recovery.md), so decrypting one always
// needs the recovery identity, live envelope or not. Drill that leg manually
// with `exocomp restore --identity`.
func LiveSource(env *secrets.Envelope, t store.BackupTarget, targetEnv func(store.BackupTarget) map[string]string) (Source, error) {
	if t.Kind == KindGit {
		return Source{}, fmt.Errorf("backup: drill: target %q is a git target; "+
			"live drilling has no identity to decrypt it (drill it manually with "+
			"`exocomp restore --from %s --identity <file>`)", t.Name, t.URI)
	}

	key := env.Derive("restic-repo", t.Name)
	if len(key) != dekLen {
		return Source{}, errors.New("backup: drill: envelope is closed")
	}

	var extra map[string]string
	if targetEnv != nil {
		extra = targetEnv(t)
	}
	return Source{
		Kind:     t.Kind,
		URI:      t.URI,
		Name:     t.Name,
		Password: base64.StdEncoding.EncodeToString(key),
		Env:      extra,
	}, nil
}

// drillOutcomeRaw builds the structured payload a drill result's outcome event
// carries: the DrillEvent envelope with the source scrubbed of any embedded
// userinfo exactly like the human line, so no credential can ride the payload
// (the queue's redactor scrubs it again before the store write). DrillOutcomeFor
// (drill.go) reads it back.
func drillOutcomeRaw(result DrillResult) (string, error) {
	raw, err := json.Marshal(map[string]DrillEvent{drillEventKey: {
		Source:        ScrubURIUserinfo(result.Source),
		SnapshotID:    result.SnapshotID,
		RPOAgeSeconds: int64(result.RPOAge.Seconds()),
		OK:            result.OK,
		Reason:        result.Reason,
		VerifiedFiles: result.VerifiedFiles,
		VerifiedBytes: result.VerifiedBytes,
	}})
	if err != nil {
		return "", fmt.Errorf("backup: drill: encode outcome: %w", err)
	}
	return string(raw), nil
}

// runDrill implements the DrillJobKind leg of Runner.Run: one target's
// restore drill, fired by a kind-drill schedule fire. A drill never writes a
// backup_run row — it is not a backup — so its only visible trace is this
// job's own state and events; a bad backup (DrillResult.OK false) settles the
// job failed exactly as a bad backup leg does, which is what lets the
// existing job-failure sweep (internal/reactive) page on it unchanged.
func (r *Runner) runDrill(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	started := time.Now()

	name, err := DrillTargetOf(spec.Argv)
	if err != nil {
		return executor.Result{}, err
	}

	row, err := r.store.GetBackupTarget(ctx, name)
	if err != nil {
		return executor.Result{}, fmt.Errorf("backup: drill: target %q: %w", name, err)
	}
	if !row.Enabled {
		return executor.Result{}, fmt.Errorf("backup: drill: target %q is disabled", name)
	}

	src, err := LiveSource(r.env, row, r.targetEnv)
	if err != nil {
		return executor.Result{}, err
	}
	src.Driver = r.driver

	result, err := RestoreDrill(ctx, src, r.stageRoot)
	if err != nil {
		return executor.Result{}, fmt.Errorf("backup: drill: %w", err)
	}

	level := executor.LevelInfo
	exitCode := 0
	if !result.OK {
		level = executor.LevelError
		exitCode = 1
	}
	// result.Source carries "<kind>:<uri>", and a git/restic target's uri can
	// embed userinfo (add-target warns but does not refuse it) — scrub it the
	// same way the git and restic legs scrub their own recorded output. The
	// structured payload rides the SAME event, its own copy of the source
	// scrubbed by drillOutcomeRaw, so the log keeps the human line and the web
	// gets the machine-readable outcome together.
	raw, err := drillOutcomeRaw(result)
	if err != nil {
		return executor.Result{}, err
	}
	sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: level, Msg: ScrubURIUserinfo(result.String()), RawJSON: raw})
	return executor.Result{ExitCode: exitCode, Duration: time.Since(started)}, nil
}
