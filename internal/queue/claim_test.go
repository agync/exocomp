package queue_test

import (
	"context"
	"errors"
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

// TestClaimSkipsPastAWindowOfBlockedRows is the starvation case: a run of
// queued rows sharing one held concurrency key, wider than claim's one-page
// scan, must not hide a runnable job sitting behind it. Before the fix, claim
// fetched only the oldest ClaimPageSize queued rows and gave up once it had
// tried every one of them — a window entirely filled with held-key rows meant
// the runnable job past the window was never even looked at.
func TestClaimSkipsPastAWindowOfBlockedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "exocomp.db"))

	blocked := store.NewJob{
		Runbook: "backup", Kind: "backup", Trigger: "manual",
		Args:           store.JobArgs{Argv: []string{"/bin/true"}},
		ConcurrencyKey: "block",
	}
	holder, err := st.CreateJob(ctx, blocked)
	if err != nil {
		t.Fatalf("create holder: %v", err)
	}
	if err := st.ClaimJob(ctx, holder.ID); err != nil {
		t.Fatalf("claim holder: %v", err)
	}
	// A full page of rows sharing the held key: every one of them refuses to
	// claim, and never frees, for the life of this test.
	for range queue.ClaimPageSize {
		if _, err := st.CreateJob(ctx, blocked); err != nil {
			t.Fatalf("create blocked row: %v", err)
		}
	}

	q := queueOver(st, executor.NewShell(), 2)
	runnable, err := q.Enqueue(ctx, shellSpec("/bin/true"))
	if err != nil {
		t.Fatalf("enqueue runnable: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- q.Run(runCtx) }()

	waitForState(t, st, runnable.ID, store.StateSucceeded)

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run never returned after cancel")
	}

	// The blocked rows were never candidates: the holder never finished.
	for _, id := range []int64{holder.ID} {
		got, err := st.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("get holder: %v", err)
		}
		if got.State != store.StateRunning {
			t.Errorf("holder %d state = %s, want still running", id, got.State)
		}
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

// TestWithoutDrainSetTracksNothing is the daemon's shape: a queue built with
// WithoutDrainSet that only ever Runs. Its claim never consults the set, so
// an idempotency hit on a job it will never itself claim — still queued,
// already claimed by another process, or long since terminal — must leave
// nothing behind; the old behavior remembered every one of these and only
// ever forgot a job this instance actually launched, which is a leak with no
// upper bound over a daemon's lifetime.
func TestWithoutDrainSetTracksNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "exocomp.db"))

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      2,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, queue.WithoutDrainSet())

	spec := shellSpec("/bin/true")
	spec.IdempotencyKey = "sched-fire"
	fresh, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := q.TrackedJobs(); got != 0 {
		t.Fatalf("tracked jobs after a fresh enqueue = %d, want 0", got)
	}

	// The same schedule firing again before anything has claimed the row: an
	// idempotency hit on a job still queued.
	if _, err := q.Enqueue(ctx, spec); err != nil {
		t.Fatalf("re-enqueue while queued: %v", err)
	}
	if got := q.TrackedJobs(); got != 0 {
		t.Fatalf("tracked jobs after a queued idempotency hit = %d, want 0", got)
	}

	// A row another process claimed: this instance's own claim loop, which
	// only ever looks at queued rows, will never see it to forget it.
	elsewhere, err := st.CreateJob(ctx, store.NewJob{
		Runbook:        "devrun",
		Kind:           "shell",
		Trigger:        "schedule",
		Args:           store.JobArgs{Argv: []string{"/bin/sleep", "30"}},
		IdempotencyKey: "sched-elsewhere",
	})
	if err != nil {
		t.Fatalf("create elsewhere job: %v", err)
	}
	if err := st.ClaimJob(ctx, elsewhere.ID); err != nil {
		t.Fatalf("claim elsewhere job: %v", err)
	}
	elsewhereSpec := shellSpec("/bin/sleep", "30")
	elsewhereSpec.IdempotencyKey = "sched-elsewhere"
	if _, err := q.Enqueue(ctx, elsewhereSpec); err != nil {
		t.Fatalf("enqueue hitting an elsewhere-claimed job: %v", err)
	}
	if got := q.TrackedJobs(); got != 0 {
		t.Fatalf("tracked jobs after an elsewhere-claimed idempotency hit = %d, want 0", got)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- q.Run(runCtx) }()

	waitForState(t, st, fresh.ID, store.StateSucceeded)
	if got := q.TrackedJobs(); got != 0 {
		t.Fatalf("tracked jobs once the daemon claimed and ran its own job = %d, want 0", got)
	}

	// The schedule keeps firing on the now-terminal key for the rest of the
	// daemon's life: the case that leaked one entry per hit before.
	for range 20 {
		if _, err := q.Enqueue(ctx, spec); err != nil {
			t.Fatalf("post-terminal re-enqueue: %v", err)
		}
	}
	if got := q.TrackedJobs(); got != 0 {
		t.Fatalf("tracked jobs after 20 post-terminal idempotency hits = %d, want 0", got)
	}

	cancel()
	select {
	case err := <-done:
		// A cancel landing between dispatch's own store call and the loop's
		// ctx.Done() check surfaces as a wrapped context.Canceled; serve.go
		// treats that the same as the clean nil return on shutdown.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run never returned after cancel")
	}
	if got := q.TrackedJobs(); got != 0 {
		t.Fatalf("tracked jobs after the daemon stopped = %d, want 0", got)
	}
}
