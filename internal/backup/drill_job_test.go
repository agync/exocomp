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

// TestRunnerDrillCatchesCorruption proves a bad backup settles the drill job
// failed (exit 1, no error) rather than erroring: the drill did its job, and
// that outcome is what lets the ordinary job-failure sweep page on it.
func TestRunnerDrillCatchesCorruption(t *testing.T) {
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
	for _, e := range events() {
		if e.Level == executor.LevelError && strings.Contains(e.Msg, "restore drill FAILED") {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Errorf("no error-level 'restore drill FAILED' event: %+v", events())
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
