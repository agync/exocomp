package queue_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// openStore opens a database at path, shared by the queues a test builds over
// it.
func openStore(t *testing.T, path string) *store.Store {
	t.Helper()

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func queueOver(st *store.Store, drv executor.Driver, workers int) *queue.Queue {
	return queue.New(st, drv, queue.Options{
		Workers:      workers,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// TestDrainLeavesForeignRowsAlone is the CLI half of the claim filter. A row
// another process queued belongs to that process's driver: a drain that
// claimed it would run it through the wrong one and fail a job it has no
// business touching.
func TestDrainLeavesForeignRowsAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "exocomp.db"))

	foreign, err := st.CreateJob(ctx, store.NewJob{
		Runbook: "someone-else",
		Kind:    "tofu",
		Trigger: "schedule",
		Args:    store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create foreign job: %v", err)
	}

	q := queueOver(st, executor.NewShell(), 4)
	mine, err := q.Enqueue(ctx, shellSpec("/bin/true"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	drain(t, q)

	if got, err := st.GetJob(ctx, mine.ID); err != nil || got.State != store.StateSucceeded {
		t.Fatalf("own job is %s (%v), want succeeded", got.State, err)
	}
	got, err := st.GetJob(ctx, foreign.ID)
	if err != nil {
		t.Fatalf("get foreign job: %v", err)
	}
	if got.State != store.StateQueued {
		t.Errorf("the drain moved a foreign job to %s; it must stay queued", got.State)
	}
	events, err := st.ListJobEvents(ctx, foreign.ID)
	if err != nil {
		t.Fatalf("list foreign events: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("the drain wrote %d event(s) on a foreign job", len(events))
	}
}

// TestDrainIdlesWhileForeignRowsKeepArriving is the other half: a daemon
// enqueueing on a schedule never lets the job table go empty, so a drain that
// waits for global emptiness never returns.
func TestDrainIdlesWhileForeignRowsKeepArriving(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "exocomp.db"))

	// Long-running foreign work, and more of it arriving: a drain that claims
	// what it did not enqueue blocks on these instead of returning.
	foreign := store.NewJob{
		Runbook: "ticker",
		Kind:    "shell",
		Trigger: "schedule",
		Args:    store.JobArgs{Argv: []string{"/bin/sleep", "60"}},
	}
	for range 3 {
		if _, err := st.CreateJob(ctx, foreign); err != nil {
			t.Fatalf("create foreign job: %v", err)
		}
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				_, _ = st.CreateJob(ctx, foreign)
			}
		}
	}()

	q := queueOver(st, executor.NewShell(), 2)
	if _, err := q.Enqueue(ctx, shellSpec("/bin/true")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- q.Drain(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("drain never returned while another process kept enqueueing")
	}
}

// TestConcurrencyKeyHoldsAcrossQueues is the cross-process guarantee, proved
// with two queues over one database: neither one's memory can see the other's,
// so only the store can serialize them.
func TestConcurrencyKeyHoldsAcrossQueues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")
	first := openStore(t, path)
	second := openStore(t, path)

	markers := filepath.Join(t.TempDir(), "markers")
	spec := shellSpec(markerScript(markers)...)
	spec.ConcurrencyKey = "backup:nas"

	qa := queueOver(first, executor.NewShell(), 2)
	qb := queueOver(second, executor.NewShell(), 2)
	if _, err := qa.Enqueue(ctx, spec); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if _, err := qb.Enqueue(ctx, spec); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	done := make(chan error, 2)
	go func() { done <- qa.Drain(ctx) }()
	go func() { done <- qb.Drain(ctx) }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a drain never returned")
		}
	}

	lines := readMarkers(t, markers)
	if len(lines) != 4 {
		t.Fatalf("markers = %q, want 4 lines", lines)
	}
	for i, line := range lines {
		want := "start"
		if i%2 == 1 {
			want = "end"
		}
		if line != want {
			t.Fatalf("markers = %q: two processes ran one concurrency key at once", lines)
		}
	}
}

// TestClaimRefusesAHeldConcurrencyKey pins the store contract the queue leans
// on: the queued -> running move and the key check are one statement.
func TestClaimRefusesAHeldConcurrencyKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "exocomp.db"))

	newJob := store.NewJob{
		Runbook:        "backup",
		Kind:           "backup",
		Trigger:        "manual",
		Args:           store.JobArgs{Argv: []string{"/bin/true"}},
		ConcurrencyKey: "backup:nas",
	}
	held, err := st.CreateJob(ctx, newJob)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	waiting, err := st.CreateJob(ctx, newJob)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	if err := st.ClaimJob(ctx, held.ID); err != nil {
		t.Fatalf("claim the first job: %v", err)
	}
	err = st.ClaimJob(ctx, waiting.ID)
	if err == nil {
		t.Fatal("the store claimed a job whose concurrency key is held")
	}
	if !strings.Contains(err.Error(), "concurrency key") {
		t.Errorf("claim error = %v, want one naming the concurrency key", err)
	}
	if got, err := st.GetJob(ctx, waiting.ID); err != nil || got.State != store.StateQueued {
		t.Errorf("refused job is %s (%v), want queued", got.State, err)
	}

	// The key frees when the holder ends, and only then.
	if err := st.TransitionJob(ctx, held.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("finish the holder: %v", err)
	}
	if err := st.ClaimJob(ctx, waiting.ID); err != nil {
		t.Fatalf("claim after the key was freed: %v", err)
	}
}

// TestTheClaimSetShrinksAsJobsEnd: the daemon's queue enqueues every schedule
// fire for as long as the process lives. A set that only grew would be a leak
// measured in days.
func TestTheClaimSetShrinksAsJobsEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "exocomp.db"))
	q := queueOver(st, executor.NewShell(), 2)

	for range 8 {
		if _, err := q.Enqueue(ctx, shellSpec("/bin/true")); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if got := q.TrackedJobs(); got != 8 {
		t.Fatalf("tracked jobs before the drain = %d, want 8", got)
	}

	drain(t, q)

	if got := q.TrackedJobs(); got != 0 {
		t.Errorf("tracked jobs after every job ended = %d, want 0", got)
	}
}
