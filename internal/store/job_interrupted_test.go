package store_test

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestListInterruptedJobs is the post-reboot listing: only interrupted rows come
// back, newest first, and a still-live job is not among them.
func TestListInterruptedJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := open(t)

	mk := func(runbook string) store.Job {
		j, err := s.CreateJob(ctx, store.NewJob{
			Runbook: runbook, Kind: "shell", Trigger: "manual",
			Args: store.JobArgs{Argv: []string{"/bin/true"}},
		})
		if err != nil {
			t.Fatalf("create %s: %v", runbook, err)
		}
		return j
	}
	interrupt := func(j store.Job) {
		if err := s.TransitionJob(ctx, j.ID, store.StateQueued, store.StateInterrupted); err != nil {
			t.Fatalf("interrupt %d: %v", j.ID, err)
		}
	}

	first := mk("first")
	second := mk("second")
	live := mk("live") // stays queued

	interrupt(first)
	interrupt(second)

	got, err := s.ListInterruptedJobs(ctx, 0)
	if err != nil {
		t.Fatalf("list interrupted: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d interrupted jobs, want 2", len(got))
	}
	// Newest interruption first.
	if got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("order = [%d %d], want [%d %d] (newest first)",
			got[0].ID, got[1].ID, second.ID, first.ID)
	}
	for _, j := range got {
		if j.State != store.StateInterrupted {
			t.Fatalf("job %d is %s in the interrupted listing", j.ID, j.State)
		}
		if j.ID == live.ID {
			t.Fatalf("a queued job leaked into the interrupted listing")
		}
	}
}
