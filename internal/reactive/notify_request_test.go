package reactive_test

import (
	"context"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// registerNotify writes the durable one-shot notify request the web handler
// writes, for the job the daemon should page on when it goes terminal.
func (h *harness) registerNotify(t *testing.T, jobID int64) {
	t.Helper()
	if _, err := h.store.CreateEvent(context.Background(), store.Event{
		Source:   reactive.SourceNotifyRequest,
		DedupKey: reactive.NotifyRequestDedupKey(jobID),
		Outcome:  store.OutcomeMatched,
		Reason:   "one-shot notify request for job",
		JobID:    &jobID,
	}); err != nil {
		t.Fatalf("register notify: %v", err)
	}
}

// TestNotifyRequestPagesOnTerminal: a one-shot request pages once when its job
// reaches a terminal state — including a clean success the failure sweep never
// pages on — and never again on a later sweep.
func TestNotifyRequestPagesOnTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: reactive.WorkspaceRunbook("prod"), Kind: executor.KindTofu, Trigger: "manual",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	h.registerNotify(t, job.ID)

	// Still running: nothing to page on yet.
	if paged, err := h.engine.SweepJobNotifyRequests(ctx); err != nil || paged != 0 {
		t.Fatalf("pre-terminal sweep = (%d, %v), want 0 pages", paged, err)
	}

	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("start job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("finish job: %v", err)
	}

	paged, err := h.engine.SweepJobNotifyRequests(ctx)
	if err != nil || paged != 1 {
		t.Fatalf("terminal sweep = (%d, %v), want 1 page", paged, err)
	}
	if len(h.sent.sent) != 1 || !strings.Contains(h.sent.sent[0].Title, "succeeded") {
		t.Fatalf("notify page = %+v, want one 'succeeded' page", h.sent.sent)
	}

	// Idempotent: the shared checkpoint stops a second page.
	if again, err := h.engine.SweepJobNotifyRequests(ctx); err != nil || again != 0 {
		t.Fatalf("second sweep = (%d, %v), want 0 pages", again, err)
	}
	if len(h.sent.sent) != 1 {
		t.Fatalf("transport saw %d notifications after the second sweep, want 1", len(h.sent.sent))
	}
}

// TestNotifyRequestNoDoubleFireOnFailure: a requested job that also fails is
// paged once — the failure page — never a second time by the notify sweep,
// because both share the SourceJobOutcome checkpoint.
func TestNotifyRequestNoDoubleFireOnFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	failed := h.finishedJob(t, reactive.WorkspaceRunbook("prod"), store.StateFailed, "tofu apply exited 1")
	h.registerNotify(t, failed.ID)

	// The failure sweep pages first (serve.go runs it before the notify sweep).
	if paged, err := h.engine.SweepJobOutcomes(ctx); err != nil || paged != 1 {
		t.Fatalf("failure sweep = (%d, %v), want 1 page", paged, err)
	}
	// The notify sweep sees the checkpoint and stays silent.
	if paged, err := h.engine.SweepJobNotifyRequests(ctx); err != nil || paged != 0 {
		t.Fatalf("notify sweep after failure = (%d, %v), want 0 pages", paged, err)
	}
	if len(h.sent.sent) != 1 {
		t.Fatalf("transport saw %d notifications, want exactly 1 (the failure page)", len(h.sent.sent))
	}
}
