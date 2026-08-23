package queue_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// TestClaimGateStopsNewClaimsNotRunningJobs is the cordon guarantee at the queue
// level: while the gate is closed the daemon claims no NEW job, but a job already
// running is left to finish — cordon drains, it does not cancel.
func TestClaimGateStopsNewClaimsNotRunningJobs(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := openStore(t, filepath.Join(t.TempDir(), "exocomp.db"))

	var open atomic.Bool
	open.Store(true)
	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      4,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, queue.WithClaimGate(func() bool { return open.Load() }))

	done := make(chan error, 1)
	go func() { done <- q.Run(ctx) }()

	// A long job claimed while the gate is open, so it is in-flight when we cordon.
	running, err := q.Enqueue(ctx, shellSpec("/bin/sleep", "30"))
	if err != nil {
		t.Fatalf("enqueue running: %v", err)
	}
	waitForState(t, st, running.ID, store.StateRunning)

	// Cordon, and let any claim iteration already past its gate check settle
	// before enqueuing, so the assertion tests the cordon and not that window.
	open.Store(false)
	time.Sleep(100 * time.Millisecond)

	// A job enqueued while cordoned must not be claimed.
	blocked, err := q.Enqueue(ctx, shellSpec("/bin/true"))
	if err != nil {
		t.Fatalf("enqueue blocked: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got, _ := st.GetJob(ctx, blocked.ID); got.State != store.StateQueued {
		t.Fatalf("cordoned queue claimed a new job (state %s); it must stay queued", got.State)
	}
	// And the in-flight job was NOT cancelled by the cordon.
	if got, _ := st.GetJob(ctx, running.ID); got.State != store.StateRunning {
		t.Fatalf("cordon moved an in-flight job to %s; it must keep running", got.State)
	}

	// Uncordon: the blocked job is now claimed and runs to completion.
	open.Store(true)
	waitForState(t, st, blocked.ID, store.StateSucceeded)

	cancel()
	// Run surfaces the shutdown cancellation as context.Canceled, the same value
	// the daemon's shutdown path expects and filters (serve.go hasNonCancelError).
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
}
