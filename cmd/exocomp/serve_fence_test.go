package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// These tests exercise the cross-host fence with two daemon identities over ONE
// store — the split-brain the host-local flock cannot see. Goroutines stand in
// for the two hosts (the flock's same-host guard is process-level and covered by
// TestServeRefusesASecondInstance); what is under test here is the lease that
// keeps a second host from going active against a replicated copy of the store.

// tagDriver records which daemon (by holder tag) ran each job, so a test can
// prove the active daemon claimed the work and the fenced-out one claimed none.
type tagDriver struct {
	holder string
	mu     *sync.Mutex
	ran    map[int64]string
}

func (d tagDriver) Run(_ context.Context, spec executor.Spec, _ executor.EventSink) (executor.Result, error) {
	d.mu.Lock()
	d.ran[spec.JobID] = d.holder
	d.mu.Unlock()
	return executor.Result{ExitCode: 0}, nil
}

func openLeaseStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func waitJobState(t *testing.T, st *store.Store, id int64, want store.JobState) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		job, err := st.GetJob(context.Background(), id)
		if err != nil {
			t.Fatalf("get job %d: %v", id, err)
		}
		if job.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %d never reached %s", id, want)
}

// TestServeLeaseFencesCrossHost is the fence gate: with one daemon holding a
// live lease over a store, a second daemon on another host refuses to become
// active (naming the holder and its last renew) and claims no jobs; force-taking
// the live lease needs the explicit override, and once taken the original
// holder's renew loses so it stands down.
func TestServeLeaseFencesCrossHost(t *testing.T) {
	ctx := context.Background()
	st := openLeaseStore(t)

	const ttl, renew = time.Minute, 10 * time.Second

	// Daemon A becomes the active one.
	fenceA, err := acquireLease(ctx, st, "host-a/pid-1", ttl, renew, false, io.Discard)
	if err != nil {
		t.Fatalf("daemon A acquire: %v", err)
	}

	// A's queue is the only active one; it claims what is enqueued.
	var mu sync.Mutex
	ran := map[int64]string{}
	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	qA := queue.New(st, tagDriver{holder: "host-a", mu: &mu, ran: ran}, queue.Options{
		Workers:      2,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	doneA := make(chan error, 1)
	go func() { doneA <- qA.Run(ctxA) }()

	// Daemon B, on another host, tries to start while A's lease is live: it must
	// REFUSE rather than double-activate — and say who holds the store.
	_, errB := acquireLease(ctx, st, "host-b/pid-2", ttl, renew, false, io.Discard)
	if errB == nil {
		t.Fatal("daemon B became active while A's lease was live")
	}
	if !strings.Contains(errB.Error(), "host-a/pid-1") {
		t.Errorf("refusal did not name the holder: %v", errB)
	}
	if !strings.Contains(errB.Error(), "renewed") {
		t.Errorf("refusal did not report when the lease was last renewed: %v", errB)
	}

	// B refused before starting a queue, so only A claims work: the tag proves it.
	job, err := qA.Enqueue(ctx, queue.Spec{Runbook: "devrun", Kind: "shell", Trigger: "test", Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitJobState(t, st, job.ID, store.StateSucceeded)
	mu.Lock()
	who := ran[job.ID]
	mu.Unlock()
	if who != "host-a" {
		t.Fatalf("job ran under %q, want the sole active daemon host-a", who)
	}

	// Force-taking a live lease is the explicit override. Once B takes it, A's
	// renew loses — the signal its renew loop uses to stand down.
	if _, err := acquireLease(ctx, st, "host-b/pid-2", ttl, renew, true, io.Discard); err != nil {
		t.Fatalf("force-take a live lease: %v", err)
	}
	if err := st.RenewLease(ctx, "host-a/pid-1", time.Now().UTC()); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("displaced holder's renew: %v, want ErrLeaseLost", err)
	}

	cancelA()
	if err := <-doneA; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("queue A run: %v", err)
	}
	_ = fenceA
}

// TestServeLeaseTakenAfterRelease proves a standby becomes active once the
// primary's lease is free: released on a clean stop (no TTL wait), and — the
// ungraceful path — once it goes stale past its TTL.
func TestServeLeaseTakenAfterRelease(t *testing.T) {
	ctx := context.Background()
	st := openLeaseStore(t)

	// Clean shutdown: A releases, B takes over at once, well within any TTL.
	fenceA, err := acquireLease(ctx, st, "host-a/pid-1", time.Minute, 10*time.Second, false, io.Discard)
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	fenceA.release(ctx, io.Discard)
	if _, err := acquireLease(ctx, st, "host-b/pid-2", time.Minute, 10*time.Second, false, io.Discard); err != nil {
		t.Fatalf("B acquire after A released: %v", err)
	}

	// Ungraceful loss: B holds a short-TTL lease and stops renewing (goroutine
	// gone). Once past the TTL it is stale, and a standby takes it without force.
	st2 := openLeaseStore(t)
	if _, err := acquireLease(ctx, st2, "host-b/pid-2", 150*time.Millisecond, 50*time.Millisecond, false, io.Discard); err != nil {
		t.Fatalf("short-lease acquire: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := acquireLease(ctx, st2, "host-c/pid-3", time.Minute, 10*time.Second, false, io.Discard); err != nil {
		t.Fatalf("standby acquire after TTL lapse: %v", err)
	}
}
