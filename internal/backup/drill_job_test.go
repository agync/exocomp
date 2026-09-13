package backup

import (
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

func TestDrillArgvTargetOfRoundTrip(t *testing.T) {
	t.Parallel()
	got, err := DrillTargetOf(DrillArgv("nas"))
	if err != nil || got != "nas" {
		t.Fatalf("DrillTargetOf(DrillArgv(%q)) = %q, %v", "nas", got, err)
	}
}

func TestDrillTargetOfRejectsBadArgv(t *testing.T) {
	t.Parallel()
	bad := [][]string{
		nil,
		{"exocomp-drill"},
		{"exocomp-backup", "nas"}, // a real backup argv, not a drill one
		{"exocomp-drill", "nas", "extra"},
	}
	for _, argv := range bad {
		if _, err := DrillTargetOf(argv); err == nil {
			t.Errorf("DrillTargetOf(%v): want error", argv)
		}
	}
}

// TestRunDrillScrubsCredentialSource proves the message and the structured
// payload runDrill hands to sink.Emit never carry a target's embedded
// credential. DrillResult.Source is built as "<kind>:<uri>" (drill.go)
// exactly like a git/restic leg's own recorded output (git.go, restic.go),
// so a drill result for a cred-bearing target must go through the same
// ScrubURIUserinfo before it reaches a job_event row — a real restic_s3
// target can carry userinfo in its endpoint (resticRepo passes an
// "s3:https://user:pass@host/bucket" uri through unparsed), which is what
// this reproduces without needing a reachable S3 endpoint.
func TestRunDrillScrubsCredentialSource(t *testing.T) {
	t.Parallel()
	const sentinel = "s3kr1t-p4ss-6e21" //nolint:gosec // test sentinel, not a real credential
	result := DrillResult{
		OK:         true,
		Source:     KindResticS3 + ":s3:https://bot:" + sentinel + "@s3.invalid/bucket",
		SnapshotID: "2026-08-22T00:00:00Z",
	}
	raw := result.String()
	if !strings.Contains(raw, sentinel) {
		t.Fatalf("test setup: sentinel not present in the unscrubbed message: %q", raw)
	}

	msg := ScrubURIUserinfo(raw)
	if strings.Contains(msg, sentinel) {
		t.Fatalf("drill result message leaked the credential: %q", msg)
	}
	if !strings.Contains(msg, "***@") {
		t.Errorf("scrubbed message missing the redaction marker: %q", msg)
	}

	payload, err := drillOutcomeRaw(result)
	if err != nil {
		t.Fatalf("drillOutcomeRaw: %v", err)
	}
	if strings.Contains(payload, sentinel) {
		t.Fatalf("drill payload leaked the credential: %q", payload)
	}
	outcome, ok := DrillOutcomeFor(payload)
	if !ok {
		t.Fatal("drill payload did not read back: " + payload)
	}
	if outcome.Source != KindResticS3+":s3:https://***@s3.invalid/bucket" {
		t.Errorf("payload source not scrubbed: %q", outcome.Source)
	}
	if outcome.VerifiedBytes != 0 {
		t.Errorf("payload verified bytes = %d, want 0 for a result that never ran", outcome.VerifiedBytes)
	}
}

func TestDrillConcurrencyKeyFormat(t *testing.T) {
	t.Parallel()
	if got := DrillConcurrencyKey("nas"); got != "drill:nas" {
		t.Errorf("DrillConcurrencyKey(%q) = %q, want %q", "nas", got, "drill:nas")
	}
}

// TestLiveSourceRefusesGitTarget: a git leg is only ever encrypted to public
// recipients, so live drilling — no age identity — can never decrypt one.
func TestLiveSourceRefusesGitTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := store.BackupTarget{Name: "offsite", Kind: KindGit, URI: t.TempDir()}

	_, err := LiveSource(f.env, target, nil)
	if err == nil {
		t.Fatal("LiveSource against a git target: want error")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("error does not name the git restriction: %v", err)
	}
}

// TestLiveSourceMatchesEnginePassword pins LiveSource's derivation against
// the engine's own resticPassword, via a real backup + drill round trip: a
// backup written under the engine's password must open under LiveSource's.
func TestLiveSourceMatchesEnginePassword(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real restic")
	}
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))
	if _, err := f.engine.Run(t.Context(), []Target{target}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	src, err := LiveSource(f.env, target.BackupTarget, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	if err != nil {
		t.Fatalf("LiveSource: %v", err)
	}

	result, err := RestoreDrill(t.Context(), src, t.TempDir())
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if !result.OK {
		t.Fatalf("drill against a target LiveSource itself resolved: not OK: %+v", result)
	}
}

// TestRunnerDrillsRealTarget runs a real Runner (real Engine, real restic)
// through DrillJobKind end to end: seed a backup with the ordinary backup
// job, then drill it via the queue-shaped entry point a kind-drill schedule
// fire actually uses.
func TestRunnerDrillsRealTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real restic")
	}
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))

	backupRunner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, _ := collectSink()
	if _, err := backupRunner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv(target.Name)}, sink); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	drillRunner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, events := collectSink()
	res, err := drillRunner.Run(t.Context(),
		executor.Spec{Kind: DrillJobKind, Argv: DrillArgv(target.Name)}, sink)
	if err != nil {
		t.Fatalf("Run: %v (events: %+v)", err, events())
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (events: %+v)", res.ExitCode, events())
	}

	var sawOK bool
	for _, e := range events() {
		if e.Level == executor.LevelInfo && strings.Contains(e.Msg, "restore drill OK") {
			sawOK = true
		}
	}
	if !sawOK {
		t.Errorf("no info-level 'restore drill OK' event: %+v", events())
	}

	runs, err := f.store.ListBackupRuns(t.Context(), target.ID, 10)
	if err != nil {
		t.Fatalf("list backup runs: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("backup_run rows = %d after a drill, want 1 (the seed backup only): a drill wrote one", len(runs))
	}
}

// TestRunnerDrillEmitsStructuredOutcome: a successful drill's outcome event
// carries a parseable raw_json payload (the backup.drill envelope, DrillEvent)
// with the right verified counts and ok, on the SAME event whose Msg keeps the
// human line — the A4 bytes fact's source now that the web no longer
// text-parses the line.
func TestRunnerDrillEmitsStructuredOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real restic")
	}
	t.Parallel()
	f := newFixture(t)
	repo := filepath.Join(t.TempDir(), "repo")
	target := f.target(t, "nas", KindResticLocal, repo)

	backupRunner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, _ := collectSink()
	if _, err := backupRunner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv(target.Name)}, sink); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	drillRunner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, events := collectSink()
	res, err := drillRunner.Run(t.Context(),
		executor.Spec{Kind: DrillJobKind, Argv: DrillArgv(target.Name)}, sink)
	if err != nil {
		t.Fatalf("Run: %v (events: %+v)", err, events())
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (events: %+v)", res.ExitCode, events())
	}

	var sawPayload bool
	for _, e := range events() {
		outcome, ok := DrillOutcomeFor(e.RawJSON)
		if !ok {
			continue
		}
		sawPayload = true
		if !outcome.OK {
			t.Errorf("outcome ok = false, want true: %+v", outcome)
		}
		if outcome.Source != KindResticLocal+":"+repo {
			t.Errorf("outcome source = %q, want %q", outcome.Source, KindResticLocal+":"+repo)
		}
		if outcome.SnapshotID == "" {
			t.Error("outcome carries no snapshot id")
		}
		if outcome.RPOAgeSeconds < 0 {
			t.Errorf("outcome rpo_age = %d, want a non-negative age", outcome.RPOAgeSeconds)
		}
		if outcome.VerifiedFiles != 3 {
			t.Errorf("verified files = %d, want 3", outcome.VerifiedFiles)
		}
		if outcome.VerifiedBytes <= 0 {
			t.Errorf("verified bytes = %d, want > 0", outcome.VerifiedBytes)
		}
		// The SAME event still carries the human line — the log stays readable.
		if !strings.Contains(e.Msg, "restore drill OK") || !strings.Contains(e.Msg, outcome.SnapshotID) {
			t.Errorf("outcome event lost the human line: %q (payload %q)", e.Msg, e.RawJSON)
		}
	}
	if !sawPayload {
		t.Errorf("no event carried a parseable drill payload: %+v", events())
	}
}

// TestRunnerDrillCatchesCorruption proves a bad backup settles the drill job
// failed (exit 1, no error) rather than erroring: the drill did its job, and
// that outcome is what lets the ordinary job-failure sweep page on it.
func TestRunnerDrillCatchesCorruption(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real restic")
	}
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))

	backupRunner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, _ := collectSink()
	if _, err := backupRunner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv(target.Name)}, sink); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	drillRunner := NewRunner(f.store, f.env, Options{
		Driver: corruptingRestoreDriver{inner: executor.NewShell()},
	}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, events := collectSink()
	res, err := drillRunner.Run(t.Context(),
		executor.Spec{Kind: DrillJobKind, Argv: DrillArgv(target.Name)}, sink)
	if err != nil {
		t.Fatalf("a corrupt backup must settle the job failed, not error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}

	var sawFailure bool
	var sawPayload bool
	for _, e := range events() {
		if e.Level == executor.LevelError && strings.Contains(e.Msg, "restore drill FAILED") {
			sawFailure = true
		}
		outcome, ok := DrillOutcomeFor(e.RawJSON)
		if !ok {
			continue
		}
		sawPayload = true
		if outcome.OK {
			t.Errorf("outcome ok = true for a corrupt snapshot: %+v", outcome)
		}
		if outcome.Reason == "" {
			t.Error("failed outcome carries no reason")
		}
		if !strings.Contains(e.Msg, "restore drill FAILED") {
			t.Errorf("failed outcome event lost the human line: %q", e.Msg)
		}
	}
	if !sawFailure {
		t.Errorf("no error-level 'restore drill FAILED' event: %+v", events())
	}
	if !sawPayload {
		t.Errorf("no event carried a parseable drill payload: %+v", events())
	}
}

func TestRunnerDrillRejectsGitTarget(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	if _, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "offsite", Kind: KindGit, URI: t.TempDir(), Enabled: true,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	runner := NewRunner(st, nil, Options{}, nil)
	sink, _ := collectSink()

	_, err := runner.Run(t.Context(), executor.Spec{Kind: DrillJobKind, Argv: DrillArgv("offsite")}, sink)
	if err == nil {
		t.Fatal("drill against a git target: want error")
	}
}

func TestRunnerDrillRejectsDisabledTarget(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	if _, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "off", Kind: KindResticLocal, URI: t.TempDir(), Enabled: false,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	runner := NewRunner(st, nil, Options{}, nil)
	sink, _ := collectSink()

	_, err := runner.Run(t.Context(), executor.Spec{Kind: DrillJobKind, Argv: DrillArgv("off")}, sink)
	if err == nil {
		t.Fatal("drill against a disabled target: want error")
	}
}

func TestRunnerDrillRejectsMalformedArgv(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	runner := NewRunner(st, nil, Options{}, nil)
	sink, _ := collectSink()

	_, err := runner.Run(t.Context(), executor.Spec{Kind: DrillJobKind, Argv: []string{"whatever"}}, sink)
	if err == nil {
		t.Fatal("drill with a malformed argv: want error")
	}
}
