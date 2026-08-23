package sched

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// failingEnqueuer fails its first failFirst Enqueue calls, then behaves like
// fakeEnqueuer. It models a transient enqueue error (a full or briefly
// unavailable queue) hitting a scheduled fire.
type failingEnqueuer struct {
	store *store.Store

	mu        sync.Mutex
	calls     int
	failFirst int
}

func (f *failingEnqueuer) Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error) {
	f.mu.Lock()
	f.calls++
	fail := f.calls <= f.failFirst
	f.mu.Unlock()
	if fail {
		return store.Job{}, errors.New("enqueue boom")
	}
	return f.store.CreateJob(ctx, store.NewJob{
		Runbook: spec.Runbook, Kind: spec.Kind, Trigger: spec.Trigger,
		Args: store.JobArgs{Argv: spec.Argv},
	})
}

// TestFailedEnqueueReFiresOccurrenceExactlyOnce is the lost-fire gate: a normal
// fire whose Enqueue fails must NOT advance last_run, so the occurrence stays
// owed and the next reboot's catch-up re-fires it — exactly once, never zero
// (silently dropped) and never twice.
func TestFailedEnqueueReFiresOccurrenceExactlyOnce(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	sc := createShellSchedule(t, st, "transient", 0) // @every 1h

	fake := &failingEnqueuer{store: st, failFirst: 1}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil { // last_run nil: no catch-up
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	// A prior fire left last_run one occurrence back.
	seedLastRun(t, st, sc.ID, time.Now().Add(-2*time.Hour))

	// The normal fire's only Enqueue fails. last_run must stay put so the
	// occurrence is still owed.
	s.dispatch(context.Background(), sc, "")

	// The next reboot's catch-up finds the occurrence still owed and re-fires it;
	// this time the queue accepts it.
	s.catchUp(context.Background())

	jobs, err := st.ListJobs(t.Context(), store.JobFilter{Runbook: sc.Name})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("dropped-then-recovered fire produced %d jobs, want exactly 1", len(jobs))
	}
}

// TestNormalFireDedupsCatchUpOfSameOccurrence is the double-fire gate: a normal
// fire that enqueues successfully but crashes before stamping last_run must be
// deduped when the next boot's catch-up re-fires the same occurrence. The normal
// path now carries the occurrence idempotency key, so the two collapse to one
// job. Uses a real queue so idempotency keys persist; the queue is never Run, so
// the jobs stay queued to be counted.
func TestNormalFireDedupsCatchUpOfSameOccurrence(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	q := queue.New(st, executor.NewShell(), queue.Options{})
	sc := createShellSchedule(t, st, "crashy", 0)

	s := New(st, q, testLogger())
	if err := s.Start(context.Background()); err != nil { // last_run nil: no catch-up
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	seeded := time.Now().Add(-2 * time.Hour)
	seedLastRun(t, st, sc.ID, seeded)

	// A normal fire enqueues job1 under its occurrence key and stamps last_run.
	s.dispatch(context.Background(), sc, "")

	// Model the crash "between a successful enqueue and the last_run stamp": the
	// job committed but the stamp did not, so last_run is still the pre-fire value
	// when the daemon comes back up.
	seedLastRun(t, st, sc.ID, seeded)

	// The next boot's catch-up re-fires the same owed occurrence. Its key must
	// collapse onto job1 rather than making a second job.
	s.catchUp(context.Background())

	jobs, err := st.ListJobs(t.Context(), store.JobFilter{Runbook: sc.Name})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("crash-then-catchup produced %d jobs, want 1 (idempotent)", len(jobs))
	}
}

// seedLastRun backdates a schedule's last_run to model a daemon that was last
// up (and last fired this schedule) at t.
func seedLastRun(t *testing.T, st *store.Store, id int64, at time.Time) {
	t.Helper()
	if err := st.SetScheduleLastRun(t.Context(), id, at); err != nil {
		t.Fatalf("seed last_run: %v", err)
	}
}

// TestCatchUpFiresOnceForAMissedOccurrence is the core gate: a schedule whose
// last_run predates an occurrence fires exactly one coalesced catch-up on Start.
func TestCatchUpFiresOnceForAMissedOccurrence(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	sc := createShellSchedule(t, st, "missed", 0) // @every 1h
	seedLastRun(t, st, sc.ID, time.Now().Add(-2*time.Hour))

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	if got := len(fake.Specs()); got != 1 {
		t.Fatalf("catch-up enqueued %d jobs, want exactly 1", got)
	}
}

// TestCatchUpCoalescesManyMissedOccurrences proves the fire is coalesced, not
// replayed: a daemon down for hundreds of intervals still fires exactly once,
// never one backup per missed hour.
func TestCatchUpCoalescesManyMissedOccurrences(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	sc := createShellSchedule(t, st, "longdown", 0) // @every 1h
	seedLastRun(t, st, sc.ID, time.Now().Add(-500*time.Hour))

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	if got := len(fake.Specs()); got != 1 {
		t.Fatalf("catch-up after 500 missed intervals enqueued %d jobs, want exactly 1", got)
	}
}

// TestNoCatchUpWhenLastRunIsRecent: a schedule that fired within the last
// interval missed nothing, so Start does no catch-up.
func TestNoCatchUpWhenLastRunIsRecent(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	sc := createShellSchedule(t, st, "current", 0) // @every 1h
	seedLastRun(t, st, sc.ID, time.Now().Add(-1*time.Minute))

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	if got := len(fake.Specs()); got != 0 {
		t.Fatalf("catch-up fired %d jobs for a recent last_run, want 0", got)
	}
}

// TestNoCatchUpForBrandNewSchedule: a schedule that has never fired (last_run
// NULL) only ever schedules forward — it must not back-fire on first Start.
func TestNoCatchUpForBrandNewSchedule(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	createShellSchedule(t, st, "brandnew", 0) // last_run stays NULL

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	if got := len(fake.Specs()); got != 0 {
		t.Fatalf("brand-new schedule back-fired %d jobs, want 0", got)
	}
}

// TestCatchUpAdvancesLastRun: after a catch-up fire, last_run moves to ~now, so
// a second (re)load sees the schedule as current and coalesces nothing more.
func TestCatchUpAdvancesLastRun(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	sc := createShellSchedule(t, st, "advances", 0)
	seedLastRun(t, st, sc.ID, time.Now().Add(-2*time.Hour))

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	before := time.Now().Add(-time.Second)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	after, err := st.GetScheduleByID(t.Context(), sc.ID)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if after.LastRun == nil || after.LastRun.Before(before) {
		t.Fatalf("last_run = %v, want advanced to ~now", after.LastRun)
	}

	// A reload now finds nothing to catch up.
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(fake.Specs()); got != 1 {
		t.Fatalf("a reload after catch-up fired again: total %d jobs, want 1", got)
	}
}

// TestCatchUpIsIdempotentWithARacingRefire proves the queue idempotency guard:
// two catch-up dispatches that read the same stale last_run (a SIGHUP reload
// racing the interval reload, or a crash between enqueue and last_run stamp)
// collapse to one job, not two. Uses a real queue so idempotency keys persist;
// the queue is never Run, so the jobs stay queued to be counted.
func TestCatchUpIsIdempotentWithARacingRefire(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	q := queue.New(st, executor.NewShell(), queue.Options{})
	sc := createShellSchedule(t, st, "raced", 0)
	seeded := time.Now().Add(-2 * time.Hour)
	seedLastRun(t, st, sc.ID, seeded)

	s := New(st, q, testLogger())
	if err := s.Start(context.Background()); err != nil { // first catch-up
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	// A racing evaluator (SIGHUP reload against the interval reload, or the next
	// boot after a crash between enqueue and last_run stamp) read the same stale
	// last_run the first catch-up did. Dispatching again under that anchor must
	// hit the queue's idempotency and return the first job, not make a second.
	stale := sc
	stale.LastRun = ptrTime(seeded)
	s.dispatch(context.Background(), stale, fireKey(stale))

	jobs, err := st.ListJobs(t.Context(), store.JobFilter{Runbook: sc.Name})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("racing catch-up produced %d jobs, want 1 (idempotent)", len(jobs))
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
