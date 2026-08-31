package backup

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func collectSink() (executor.EventSink, func() []executor.Event) {
	var mu sync.Mutex
	var events []executor.Event
	sink := executor.SinkFunc(func(e executor.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})
	return sink, func() []executor.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]executor.Event(nil), events...)
	}
}

func TestArgvTargetOfRoundTrip(t *testing.T) {
	t.Parallel()
	got, err := TargetOf(Argv("nas"))
	if err != nil || got != "nas" {
		t.Fatalf("TargetOf(Argv(%q)) = %q, %v", "nas", got, err)
	}
}

func TestTargetOfRejectsBadArgv(t *testing.T) {
	t.Parallel()
	bad := [][]string{
		nil,
		{"exocomp-backup"},
		{"wrong-binary", "nas"},
		{"exocomp-backup", "nas", "extra"},
	}
	for _, argv := range bad {
		if _, err := TargetOf(argv); err == nil {
			t.Errorf("TargetOf(%v): want error", argv)
		}
	}
}

func TestConcurrencyKeyFormat(t *testing.T) {
	t.Parallel()
	if got := ConcurrencyKey("nas"); got != "backup:nas" {
		t.Errorf("ConcurrencyKey(%q) = %q, want %q", "nas", got, "backup:nas")
	}
}

func TestRunnerRejectsUnknownTarget(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	runner := NewRunner(st, nil, Options{}, nil)
	sink, _ := collectSink()

	_, err := runner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv("nope")}, sink)
	if err == nil {
		t.Fatal("Run with an unregistered target: want error")
	}
}

func TestRunnerRejectsDisabledTarget(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	if _, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "off", Kind: KindResticLocal, URI: t.TempDir(), Enabled: false,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	runner := NewRunner(st, nil, Options{}, nil)
	sink, _ := collectSink()

	_, err := runner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv("off")}, sink)
	if err == nil {
		t.Fatal("Run against a disabled target: want error")
	}
}

func TestRunnerRejectsMalformedArgv(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	runner := NewRunner(st, nil, Options{}, nil)
	sink, _ := collectSink()

	_, err := runner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: []string{"whatever"}}, sink)
	if err == nil {
		t.Fatal("Run with a malformed argv: want error")
	}
}

// TestRunnerReportsLegFailureAsNonZeroExit substitutes a failing fake engine
// so the test does not need a reachable repository: only the mapping from a
// LegResult error to exit-code-only correctness (AGENTS.md invariant 3) is
// under test here.
func TestRunnerReportsLegFailureAsNonZeroExit(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	target, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "flaky", Kind: KindResticLocal, URI: t.TempDir(), Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	runner := &Runner{store: st, run: func(_ context.Context, targets []Target) (Report, error) {
		return Report{Legs: []LegResult{
			{Target: targets[0].Name, Kind: targets[0].Kind, State: store.BackupFailed, Err: errors.New("boom")},
		}}, nil
	}}
	sink, events := collectSink()

	res, err := runner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv(target.Name)}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	var sawError bool
	for _, e := range events() {
		if e.Level == executor.LevelError {
			sawError = true
		}
	}
	if !sawError {
		t.Error("no error-level event for a failed leg")
	}
}

// TestRunnerWithRealEngineWritesBackupRun runs a real Runner (real Engine,
// real restic) against a local target, proving NewRunner's wiring end to end
// and that a queued backup job leaves the backup_run row a job needs.
func TestRunnerWithRealEngineWritesBackupRun(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real restic")
	}
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "local", KindResticLocal, filepath.Join(t.TempDir(), "repo"))

	runner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, events := collectSink()

	res, err := runner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv(target.Name)}, sink)
	if err != nil {
		t.Fatalf("Run: %v (events: %+v)", err, events())
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (events: %+v)", res.ExitCode, events())
	}

	runs, err := f.store.ListBackupRuns(t.Context(), target.ID, 1)
	if err != nil {
		t.Fatalf("list backup runs: %v", err)
	}
	if len(runs) != 1 || runs[0].State != store.BackupSucceeded || runs[0].SnapshotID == "" {
		t.Fatalf("backup_run = %+v", runs)
	}
}

func TestRunnerSucceedsWithFakeEngine(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	target, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "ok", Kind: KindResticLocal, URI: t.TempDir(), Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	runner := &Runner{store: st, run: func(_ context.Context, targets []Target) (Report, error) {
		return Report{Legs: []LegResult{
			{Target: targets[0].Name, Kind: targets[0].Kind, State: store.BackupSucceeded, SnapshotID: "abc123"},
		}}, nil
	}}
	sink, _ := collectSink()

	res, err := runner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv(target.Name)}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
}

// TestConcurrentBackupJobsSerializeOnTargetKey proves the queue's
// concurrency-key mechanism actually gates two backup jobs of the same
// target, using a slow fake engine so the test needs neither a reachable
// repository nor real wall-clock backup time.
func TestConcurrentBackupJobsSerializeOnTargetKey(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	target, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "shared", Kind: KindResticLocal, URI: t.TempDir(), Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	var mu sync.Mutex
	var markers []string
	slow := func(_ context.Context, targets []Target) (Report, error) {
		mu.Lock()
		markers = append(markers, "start")
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		markers = append(markers, "end")
		mu.Unlock()
		return Report{Legs: []LegResult{
			{Target: targets[0].Name, Kind: targets[0].Kind, State: store.BackupSucceeded},
		}}, nil
	}

	runner := &Runner{store: st, run: slow}
	q := queue.New(st, runner, queue.Options{Workers: 4, PollInterval: 10 * time.Millisecond})

	for range 2 {
		if _, err := q.Enqueue(t.Context(), queue.Spec{
			Runbook: "backup", Kind: JobKind, Trigger: "manual",
			Argv: Argv(target.Name), ConcurrencyKey: ConcurrencyKey(target.Name),
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := q.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(markers) != 4 {
		t.Fatalf("markers = %v, want 4 entries", markers)
	}
	for i, m := range markers {
		want := "start"
		if i%2 == 1 {
			want = "end"
		}
		if m != want {
			t.Fatalf("markers = %v: entry %d is %q, want %q (jobs overlapped)", markers, i, m, want)
		}
	}

	jobs, err := st.ListJobs(t.Context(), store.JobFilter{ConcurrencyKey: ConcurrencyKey(target.Name)})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, j := range jobs {
		if j.State != store.StateSucceeded {
			t.Errorf("job %d state = %s, want succeeded", j.ID, j.State)
		}
	}
}
