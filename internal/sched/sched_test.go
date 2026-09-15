package sched

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// fakeEnqueuer records every Spec handed to it instead of running a queue, so
// tests can assert what a fire built without real subprocess time. It still
// creates a real (never-run) job row: schedule.last_job_id is a foreign key,
// so a fire's stamp needs one to point at.
type fakeEnqueuer struct {
	store *store.Store

	mu    sync.Mutex
	specs []queue.Spec
}

func (f *fakeEnqueuer) Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error) {
	job, err := f.store.CreateJob(ctx, store.NewJob{
		Kind: spec.Kind, Trigger: spec.Trigger,
		Args: store.JobArgs{Argv: spec.Argv},
	})
	if err != nil {
		return store.Job{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	return job, nil
}

func (f *fakeEnqueuer) Specs() []queue.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]queue.Spec(nil), f.specs...)
}

func createShellSchedule(t *testing.T, st *store.Store, name string, jitter int) store.Schedule {
	t.Helper()
	sc, err := st.CreateSchedule(t.Context(), store.NewSchedule{
		Name:     name,
		CronExpr: "@every 1h",
		Kind:     store.ScheduleShell,
		Args:     store.ScheduleArgs{Argv: []string{"/bin/true"}},
		JitterS:  jitter,
	})
	if err != nil {
		t.Fatalf("create schedule %s: %v", name, err)
	}
	return sc
}

func TestStartErrorsWhenAlreadyStarted(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	if err := s.Start(ctx); err == nil {
		t.Fatal("second Start: want error")
	}
}

func TestReloadBeforeStartErrors(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	if err := s.Reload(context.Background()); err == nil {
		t.Fatal("Reload before Start: want error")
	}
}

func TestStartSkipsDisabledSchedules(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	createShellSchedule(t, st, "on", 0)
	off := createShellSchedule(t, st, "off", 0)
	if err := st.SetScheduleEnabled(t.Context(), off.Name, false); err != nil {
		t.Fatalf("disable %s: %v", off.Name, err)
	}

	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	if got := len(s.entries); got != 1 {
		t.Fatalf("registered entries = %d, want 1 (the enabled schedule only)", got)
	}
}

func TestReloadPicksUpNewSchedule(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	if got := len(s.entries); got != 0 {
		t.Fatalf("entries before create = %d, want 0", got)
	}

	createShellSchedule(t, st, "late", 0)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(s.entries); got != 1 {
		t.Fatalf("entries after reload = %d, want 1", got)
	}
}

func TestReloadDropsNewlyDisabledSchedule(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	a := createShellSchedule(t, st, "a", 0)
	createShellSchedule(t, st, "b", 0)

	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	if got := len(s.entries); got != 2 {
		t.Fatalf("entries after start = %d, want 2", got)
	}

	if err := st.SetScheduleEnabled(t.Context(), a.Name, false); err != nil {
		t.Fatalf("disable a: %v", err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(s.entries); got != 1 {
		t.Fatalf("entries after reload = %d, want 1", got)
	}
}

func TestBuildSpecsShellRequiresArgv(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	sc := store.Schedule{Name: "empty", Kind: store.ScheduleShell}
	if _, err := s.buildSpecs(t.Context(), sc); err == nil {
		t.Fatal("shell schedule with no argv: want error")
	}
}

func TestBuildSpecsBackupSingleTarget(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	sc := store.Schedule{Name: "one", Kind: store.ScheduleBackup, Args: store.ScheduleArgs{Target: "nas"}}
	specs, err := s.buildSpecs(t.Context(), sc)
	if err != nil {
		t.Fatalf("buildSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	want := specs[0]
	if want.Kind != KindBackup || want.ConcurrencyKey != backup.ConcurrencyKey("nas") {
		t.Fatalf("spec = %+v", want)
	}
	if got, err := backup.TargetOf(want.Argv); err != nil || got != "nas" {
		t.Fatalf("argv target = %q, %v, want nas", got, err)
	}
}

func TestBuildSpecsBackupAllTargetsFansOutSkippingDisabled(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	for _, target := range []struct {
		name    string
		enabled bool
	}{
		{"nas", true},
		{"offsite", true},
		{"retired", false},
	} {
		if _, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
			Name: target.name, Kind: "restic_local", URI: t.TempDir(), Enabled: target.enabled,
		}); err != nil {
			t.Fatalf("create target %s: %v", target.name, err)
		}
	}

	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	sc := store.Schedule{Name: "all", Kind: store.ScheduleBackup}
	specs, err := s.buildSpecs(t.Context(), sc)
	if err != nil {
		t.Fatalf("buildSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2 (enabled targets only)", len(specs))
	}
	keys := map[string]bool{}
	for _, spec := range specs {
		keys[spec.ConcurrencyKey] = true
	}
	if !keys[backup.ConcurrencyKey("nas")] || !keys[backup.ConcurrencyKey("offsite")] {
		t.Fatalf("concurrency keys = %v", keys)
	}
	if keys[backup.ConcurrencyKey("retired")] {
		t.Fatal("disabled target got a spec")
	}
}

// TestSpecsMatchesFireBuild proves the exported package-level builder is the
// exact one a fire's method wrapper uses: a manual run-now enqueues the same
// work a cron fire would, with its own trigger threaded through every spec.
func TestSpecsMatchesFireBuild(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	for _, name := range []string{"nas", "offsite"} {
		if _, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
			Name: name, Kind: "restic_local", URI: t.TempDir(), Enabled: true,
		}); err != nil {
			t.Fatalf("create target %s: %v", name, err)
		}
	}
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	for _, sc := range []store.Schedule{
		{Name: "shell", Kind: store.ScheduleShell, Args: store.ScheduleArgs{Argv: []string{"/bin/true"}}, ConcurrencyKey: "k"},
		{Name: "backups", Kind: store.ScheduleBackup},
	} {
		fire, err := s.buildSpecs(t.Context(), sc)
		if err != nil {
			t.Fatalf("buildSpecs(%s): %v", sc.Name, err)
		}
		direct, err := Specs(t.Context(), st, testLogger(), sc, Trigger)
		if err != nil {
			t.Fatalf("Specs(%s): %v", sc.Name, err)
		}
		if !reflect.DeepEqual(fire, direct) {
			t.Fatalf("Specs(%s) = %+v, fire build = %+v", sc.Name, direct, fire)
		}

		// The caller's trigger must replace the fire's on every kind, shell
		// included: a manual run-now is not a cron fire.
		manual, err := Specs(t.Context(), st, testLogger(), sc, "run-now")
		if err != nil {
			t.Fatalf("Specs(%s, run-now): %v", sc.Name, err)
		}
		if len(manual) != len(direct) {
			t.Fatalf("Specs(%s, run-now) = %d specs, want %d", sc.Name, len(manual), len(direct))
		}
		for i := range manual {
			if manual[i].Trigger != "run-now" {
				t.Errorf("Specs(%s, run-now) spec %d trigger = %q, want run-now",
					sc.Name, i, manual[i].Trigger)
			}
		}
	}
}

func TestFireEnqueuesAndStampsSchedule(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	sc := createShellSchedule(t, st, "stampme", 0)

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	s.fire(sc)

	got := fake.Specs()
	if len(got) != 1 || got[0].Kind != KindShell || got[0].Trigger != Trigger {
		t.Fatalf("enqueued specs = %+v", got)
	}

	after, err := st.GetSchedule(t.Context(), sc.Name)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if after.LastJobID == nil {
		t.Fatal("last_job_id not stamped after a fire")
	}
	if after.NextAt == nil {
		t.Fatal("next_at not stamped after a fire")
	}
}

func TestFireRespectsJitterBound(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	const jitterSeconds = 1
	sc := createShellSchedule(t, st, "jittery", jitterSeconds)

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	const slack = 300 * time.Millisecond
	const rounds = 5
	for range rounds {
		started := time.Now()
		s.fire(sc)
		elapsed := time.Since(started)
		if elapsed < 0 || elapsed > jitterSeconds*time.Second+slack {
			t.Fatalf("fire took %s, want within [0, %s]", elapsed, jitterSeconds*time.Second+slack)
		}
	}
}

func TestFireStopsWaitingOnContextCancel(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	sc := createShellSchedule(t, st, "cancelled", 3600) // a jitter no test should ever sleep out

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		s.fire(sc)
		close(done)
	}()

	s.Stop() // cancels s.runCtx, which fire's jitter sleep selects on

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fire did not return after Stop cancelled its context")
	}
	if len(fake.Specs()) != 0 {
		t.Fatal("a cancelled fire still enqueued a spec")
	}
}

// TestCronFiresShellJobThroughQueue is the real-time gate scenario: an
// @every 1s schedule fires more than once through an actual queue and shell
// driver within a bounded wait.
func TestCronFiresShellJobThroughQueue(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	q := queue.New(st, executor.NewShell(), queue.Options{Workers: 4, PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = q.Run(ctx) }()

	marker := filepath.Join(t.TempDir(), "marker")
	if _, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name:     "tick",
		CronExpr: "@every 1s",
		Kind:     store.ScheduleShell,
		Args:     store.ScheduleArgs{Argv: []string{"/bin/sh", "-c", "echo tick >> " + marker}},
	}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	s := New(st, q, testLogger())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	waitForLineCount(t, marker, 2, 8*time.Second)
}

// TestRebootRebuildsSchedulesFromStore proves a schedule survives a process
// restart: a fresh Scheduler over the same store picks the row back up and
// keeps firing it, with no state kept anywhere but the database.
func TestRebootRebuildsSchedulesFromStore(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	q := queue.New(st, executor.NewShell(), queue.Options{Workers: 4, PollInterval: 20 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() { _ = q.Run(ctx) }()

	marker := filepath.Join(t.TempDir(), "marker")
	if _, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name:     "reboot-tick",
		CronExpr: "@every 1s",
		Kind:     store.ScheduleShell,
		Args:     store.ScheduleArgs{Argv: []string{"/bin/sh", "-c", "echo tick >> " + marker}},
	}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	first := New(st, q, testLogger())
	if err := first.Start(ctx); err != nil {
		t.Fatalf("start first scheduler: %v", err)
	}
	waitForLineCount(t, marker, 1, 8*time.Second)
	first.Stop()

	before := readLineCount(t, marker)

	// A new Scheduler instance over the same store stands in for the process
	// restart: nothing survives in memory, only the schedule row.
	second := New(st, q, testLogger())
	if err := second.Start(ctx); err != nil {
		t.Fatalf("start second scheduler: %v", err)
	}
	t.Cleanup(second.Stop)

	waitForLineCount(t, marker, before+1, 8*time.Second)
}

func readLineCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}

func waitForLineCount(t *testing.T, path string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if readLineCount(t, path) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("marker %s reached %d lines within %s, want >= %d", path, readLineCount(t, path), timeout, want)
}

// A reload that reads back the same rows must leave the running registrations
// — and their clocks — alone. Rebuilding restarts every entry's timer, so a
// relative expression reloaded more often than its own period would have its
// next fire pushed out forever: the schedule would never run at all.
func TestReloadDoesNotStarveARelativeSchedule(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	if _, err := st.CreateSchedule(t.Context(), store.NewSchedule{
		Name: "ticker", CronExpr: "@every 3s", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"/bin/true"}},
	}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	q := &fakeEnqueuer{store: st}
	s := New(st, q, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	// Reload far more often than the schedule's own period, which is what a
	// daemon with a reload interval shorter than a schedule does.
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.Reload(context.Background()); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if len(q.Specs()) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("a schedule reloaded more often than its period never fired")
}

// A reload that DID change something still takes effect.
func TestReloadAppliesARealChange(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	createShellSchedule(t, st, "first", 0)

	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	createShellSchedule(t, st, "second", 0)
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(s.entries); got != 2 {
		t.Fatalf("entries after a changed reload = %d, want 2", got)
	}
}
