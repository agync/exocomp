package reactive_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
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

// partialRawJSON renders the tolerated-unreachable outcome exactly as the
// ansible driver attaches it, so the fixture and the producer cannot disagree
// about the envelope key.
func partialRawJSON(t *testing.T, hosts []string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]executor.AnsiblePartialOutcome{
		"ansible_partial": {Operation: "run", Unreachable: hosts},
	})
	if err != nil {
		t.Fatalf("encode partial outcome: %v", err)
	}
	return string(raw)
}

// degradedJob writes the row a tolerated ansible run leaves behind: the
// degraded state plus the partial outcome event naming the unreachable hosts.
func (h *harness) degradedJob(t *testing.T, runbook string, hosts []string) store.Job {
	t.Helper()
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: runbook,
		Kind:    executor.KindAnsible,
		Trigger: "schedule",
		Args:    store.JobArgs{Argv: executor.AnsibleArtifactArgv("site.yml")},
	})
	if err != nil {
		t.Fatalf("create degraded job: %v", err)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "ansible: degraded — tolerated",
		RawJSON: partialRawJSON(t, hosts),
	}); err != nil {
		t.Fatalf("append partial outcome: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("start degraded job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateDegraded); err != nil {
		t.Fatalf("settle degraded job: %v", err)
	}
	return job
}

// TestNotifyRequestDegradedTitle pins D5: a one-shot request for a job that
// settles degraded pages the degraded title verbatim, naming the runbook and
// how many hosts were unreachable. failedJobStates is untouched, so the
// failure sweep never pages a degraded job — the notify page is the only one.
func TestNotifyRequestDegradedTitle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	job := h.degradedJob(t, "nightly-converge", []string{"hypnotoad", "lrrr"})
	h.registerNotify(t, job.ID)

	paged, err := h.engine.SweepJobNotifyRequests(ctx)
	if err != nil || paged != 1 {
		t.Fatalf("notify sweep = (%d, %v), want 1 page", paged, err)
	}
	note := h.sent.sent[0]
	if note.Title != "degraded: nightly-converge — 2 hosts unreachable" {
		t.Fatalf("title = %q, want the degraded title verbatim", note.Title)
	}
	if note.Priority != notify.PriorityDefault {
		t.Fatalf("priority = %d, want default — an opt-in answer, not an alarm", note.Priority)
	}

	// The failure sweep must not page a degraded job: it is not a failure.
	if paged, err := h.engine.SweepJobOutcomes(ctx); err != nil || paged != 0 {
		t.Fatalf("failure sweep paged a degraded job: (%d, %v)", paged, err)
	}
	if len(h.sent.sent) != 1 {
		t.Fatalf("transport saw %d notifications, want only the notify page", len(h.sent.sent))
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
