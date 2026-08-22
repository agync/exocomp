package queue_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestGatedJobIsNeverClaimed is the gate's central assertion. The queue runs
// as a daemon does — Run claims every queued row, whoever enqueued it — and
// still leaves the awaiting_approval row alone: the side effect the job would
// have had never happens.
//
// Mutation check: writing the gated row as queued in store.CreateGatedJob
// fails it, and so does widening both claim layers at once — (*Queue).claim's
// state filter and store.ClaimJob's predicate. Widening only one still holds,
// which is the point: the gate is two independent statements, not one.
func TestGatedJobIsNeverClaimed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 2)

	marker := filepath.Join(t.TempDir(), "ran")
	gated, err := q.EnqueueGated(ctx, shellSpec("/usr/bin/touch", marker))
	if err != nil {
		t.Fatalf("enqueue gated: %v", err)
	}
	if gated.State != store.StateAwaitingApproval {
		t.Fatalf("gated job is %s, want %s", gated.State, store.StateAwaitingApproval)
	}

	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- q.Run(runCtx) }()

	// A second, ordinary job proves the daemon really was claiming meanwhile,
	// and gives the gated row every chance to be picked up alongside it.
	witness, err := q.Enqueue(ctx, shellSpec("/bin/true"))
	if err != nil {
		t.Fatalf("enqueue witness: %v", err)
	}
	waitForState(t, st, witness.ID, store.StateSucceeded)
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}

	still, err := st.GetJob(ctx, gated.ID)
	if err != nil {
		t.Fatalf("get gated job: %v", err)
	}
	if still.State != store.StateAwaitingApproval {
		t.Fatalf("gated job is %s after a daemon run, want %s",
			still.State, store.StateAwaitingApproval)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the gated job ran: %s exists (stat err %v)", marker, err)
	}
}

func TestReleaseMakesAGatedJobRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	marker := filepath.Join(t.TempDir(), "ran")
	gated, err := q.EnqueueGated(ctx, shellSpec("/usr/bin/touch", marker))
	if err != nil {
		t.Fatalf("enqueue gated: %v", err)
	}
	if err := q.Release(ctx, gated.ID); err != nil {
		t.Fatalf("release: %v", err)
	}

	released, err := st.GetJob(ctx, gated.ID)
	if err != nil {
		t.Fatalf("get released job: %v", err)
	}
	if released.State != store.StateQueued {
		t.Fatalf("released job is %s, want queued", released.State)
	}

	// Release does not put the job in the set a Drain waits on, so the daemon
	// that owns the driver claims it. Here that is a Run with a deadline.
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- q.Run(runCtx) }()

	waitForState(t, st, gated.ID, store.StateSucceeded)
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("released job did not run: %v", err)
	}
}

func TestWithdrawRetiresAGatedJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	gated, err := q.EnqueueGated(ctx, shellSpec("/bin/true"))
	if err != nil {
		t.Fatalf("enqueue gated: %v", err)
	}
	if err := q.Withdraw(ctx, gated.ID); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	got, err := st.GetJob(ctx, gated.ID)
	if err != nil {
		t.Fatalf("get withdrawn job: %v", err)
	}
	if got.State != store.StateCancelled {
		t.Fatalf("withdrawn job is %s, want cancelled", got.State)
	}

	// A second decision on the same job finds nothing to move.
	if err := q.Release(ctx, gated.ID); err == nil {
		t.Fatal("release after withdraw: want an error, got none")
	}
}

func TestEnqueueGatedRefusesEmptyArgv(t *testing.T) {
	t.Parallel()
	q, _ := newQueue(t, 1)
	if _, err := q.EnqueueGated(context.Background(), shellSpec()); err == nil {
		t.Fatal("gated enqueue with no argv: want an error, got none")
	}
}
