package reactive_test

import (
	"context"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// finishedJob writes the row a job leaves behind once it has run to a terminal
// state, with an optional last error line the way a failed leg records one.
func (h *harness) finishedJob(t *testing.T, runbook string, state store.JobState, lastErr string) store.Job {
	t.Helper()
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: runbook,
		Kind:    executor.KindTofu,
		Trigger: "schedule",
		Args:    store.JobArgs{Argv: []string{"/bin/false"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if lastErr != "" {
		if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
			JobID: job.ID, Level: store.LevelError, Msg: lastErr,
		}); err != nil {
			t.Fatalf("append error event: %v", err)
		}
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("start job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, state); err != nil {
		t.Fatalf("finish job as %s: %v", state, err)
	}
	return job
}

// TestFailedJobPagesExactlyOnce is the loop the sweep exists for: a terminal
// failed job pages the operator once, and never again however often the sweep
// runs — the idempotency SweepDrift has, over the same event-table checkpoint.
func TestFailedJobPagesExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	failed := h.finishedJob(t, reactive.WorkspaceRunbook("prod"), store.StateFailed, "tofu apply exited 1")

	raised, err := h.engine.SweepJobOutcomes(ctx)
	if err != nil {
		t.Fatalf("sweep job outcomes: %v", err)
	}
	if raised != 1 {
		t.Fatalf("raised %d pages, want 1", raised)
	}
	if len(h.sent.sent) != 1 {
		t.Fatalf("transport saw %d notifications, want 1", len(h.sent.sent))
	}

	note := h.sent.sent[0]
	if !strings.Contains(note.Title, "job failed") {
		t.Fatalf("title = %q, want it to say the job failed", note.Title)
	}
	if !strings.Contains(note.Body, "tofu apply exited 1") {
		t.Fatalf("body = %q, want the last error in it", note.Body)
	}
	if note.Priority != notify.PriorityHigh {
		t.Fatalf("priority = %d, want high", note.Priority)
	}
	// Attributed so a scoped mute holds it.
	if note.Source != reactive.WorkspaceRunbook("prod") {
		t.Fatalf("source = %q, want the runbook", note.Source)
	}
	if note.Host != "prod" {
		t.Fatalf("host = %q, want the workspace", note.Host)
	}

	// Idempotent across repeated sweeps: the event table is the checkpoint, so
	// a second run pages nothing.
	again, err := h.engine.SweepJobOutcomes(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("second sweep raised %d pages for job %d", again, failed.ID)
	}
	if len(h.sent.sent) != 1 {
		t.Fatalf("transport saw %d notifications after the second sweep, want 1", len(h.sent.sent))
	}
}

// TestSucceededJobPagesNothing is the other half of the contract: a job that
// ran clean is not an alert.
func TestSucceededJobPagesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.finishedJob(t, reactive.WorkspaceRunbook("prod"), store.StateSucceeded, "")

	raised, err := h.engine.SweepJobOutcomes(ctx)
	if err != nil {
		t.Fatalf("sweep job outcomes: %v", err)
	}
	if raised != 0 {
		t.Fatalf("a succeeded job raised %d pages", raised)
	}
	if len(h.sent.sent) != 0 {
		t.Fatalf("transport saw %d notifications for a clean job", len(h.sent.sent))
	}
}

// TestTimedOutJobPages pins that a run killed by its own timeout is a failure
// worth paging on, alongside the plainly-failed one.
func TestTimedOutJobPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.finishedJob(t, reactive.WorkspaceRunbook("prod"), store.StateTimedOut, "deadline exceeded")

	raised, err := h.engine.SweepJobOutcomes(ctx)
	if err != nil {
		t.Fatalf("sweep job outcomes: %v", err)
	}
	if raised != 1 {
		t.Fatalf("raised %d pages for a timed-out job, want 1", raised)
	}
}
