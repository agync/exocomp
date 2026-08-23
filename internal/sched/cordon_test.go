package sched

import (
	"context"
	"testing"
	"time"
)

// TestPauseSuppressesAFireResumeReenablesIt is the scheduler half of a cordon: a
// fire that lands while paused enqueues nothing, and once resumed the next fire
// enqueues again.
func TestPauseSuppressesAFireResumeReenablesIt(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	sc := createShellSchedule(t, st, "job", 0) // @every 1h, brand-new (no last_run)

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	if got := len(fake.Specs()); got != 0 {
		t.Fatalf("a brand-new schedule caught up %d fires, want 0", got)
	}

	s.Pause()
	if !s.Paused() {
		t.Fatal("Paused() is false after Pause()")
	}
	s.fire(sc)
	if got := len(fake.Specs()); got != 0 {
		t.Fatalf("a paused scheduler fired %d jobs; a cordon must suppress the fire", got)
	}

	s.Resume()
	if s.Paused() {
		t.Fatal("Paused() is true after Resume()")
	}
	s.fire(sc)
	if got := len(fake.Specs()); got != 1 {
		t.Fatalf("after Resume the fire enqueued %d jobs, want 1", got)
	}
}

// TestResumeCatchesUpAMissedOccurrence proves Resume does not just re-enable
// firing but settles what was owed: an occurrence that passed while the schedule
// was not firing fires once (coalesced) on Resume, the same catch-up a reload
// runs — so a cordon does not silently drop a scheduled run.
func TestResumeCatchesUpAMissedOccurrence(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	sc := createShellSchedule(t, st, "owed", 0) // @every 1h
	// Fired just before the cordon: Start finds nothing owed.
	seedLastRun(t, st, sc.ID, time.Now())

	fake := &fakeEnqueuer{store: st}
	s := New(st, fake, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	if got := len(fake.Specs()); got != 0 {
		t.Fatalf("Start caught up %d fires for a just-fired schedule, want 0", got)
	}

	s.Pause()
	// An occurrence passes while paused: because a suppressed fire never advances
	// last_run, that is exactly a last_run now two hours stale relative to now.
	seedLastRun(t, st, sc.ID, time.Now().Add(-2*time.Hour))

	s.Resume()
	if got := len(fake.Specs()); got != 1 {
		t.Fatalf("Resume caught up %d fires, want exactly 1", got)
	}
}
