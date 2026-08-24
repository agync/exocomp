package store_test

import (
	"context"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// gatedJob inserts a job straight into awaiting_approval, the row a gated apply
// leaves behind. Owner is this live process unless a test reassigns it.
func gatedJob(t *testing.T, s *store.Store, runbook string) store.Job {
	t.Helper()
	job, err := s.CreateGatedJob(context.Background(), store.NewJob{
		Runbook: runbook, Kind: "tofu", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create gated job: %v", err)
	}
	if job.State != store.StateAwaitingApproval {
		t.Fatalf("gated job is %s, want awaiting_approval", job.State)
	}
	return job
}

// approve records a settled approval for a job without ever running the queue
// transition it should trigger — the shape finding #3 leaves: the human's
// answer committed, the release/withdrawal lost to a crash.
func settledApproval(t *testing.T, s *store.Store, jobID int64, to store.ApprovalState) {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateApproval(ctx, store.Approval{
		JobID:     jobID,
		ExpiresAt: time.Now().Add(time.Hour),
		TokenHash: "hash",
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	if _, err := s.DecideApproval(ctx, a.ID, to, "test", ""); err != nil {
		t.Fatalf("decide approval %s: %v", to, err)
	}
}

// TestReconcileCompletesApprovedStuckJob is finding #3, approved: DecideApproval
// committed but the release into the queue never landed. The job must not sit in
// awaiting_approval forever — the reconcile applies the settled decision.
func TestReconcileCompletesApprovedStuckJob(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	job := gatedJob(t, s, "approved-stuck")
	settledApproval(t, s, job.ID, store.ApprovalApproved)

	// The gated job is owned by this live process on purpose: a settled decision
	// is finished regardless of who owns the job, because the human already
	// answered. Owner liveness must not gate this branch.
	rec, err := s.ReconcileNonTerminal(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Released != 1 || rec.Total() != 1 {
		t.Fatalf("reconcile = %+v, want Released 1 only", rec)
	}

	got, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateQueued {
		t.Fatalf("approved-stuck job is %s, want queued", got.State)
	}
}

// TestReconcileCompletesRejectedStuckJob is finding #3 for every decision that
// settles against the job: the withdrawal that should have cancelled it was
// lost, so the reconcile cancels it. Expired and withdrawn share the branch.
func TestReconcileCompletesRejectedStuckJob(t *testing.T) {
	t.Parallel()
	for _, to := range []store.ApprovalState{
		store.ApprovalRejected, store.ApprovalExpired, store.ApprovalWithdrawn,
	} {
		to := to
		t.Run(string(to), func(t *testing.T) {
			t.Parallel()
			s, _ := open(t)
			ctx := context.Background()

			job := gatedJob(t, s, "rejected-stuck")
			settledApproval(t, s, job.ID, to)

			rec, err := s.ReconcileNonTerminal(ctx)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if rec.Cancelled != 1 || rec.Total() != 1 {
				t.Fatalf("reconcile = %+v, want Cancelled 1 only", rec)
			}

			got, err := s.GetJob(ctx, job.ID)
			if err != nil {
				t.Fatalf("get job: %v", err)
			}
			if got.State != store.StateCancelled {
				t.Fatalf("%s-stuck job is %s, want cancelled", to, got.State)
			}
		})
	}
}

// TestReconcileRetiresOrphanGatedJob is finding #4: a gated job with no approval
// row at all, the crash window between the job insert and CreateApproval. Nobody
// can ever decide it. Once its creator is gone the reconcile retires it to
// interrupted — fail-closed and re-drivable, never released and never run.
func TestReconcileRetiresOrphanGatedJob(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	job := gatedJob(t, s, "orphan-gated")
	// The creator is gone: no live process is between the insert and its (never
	// written) approval.
	if err := s.SetOwnerPIDForTest(job.ID, deadPID(t)); err != nil {
		t.Fatalf("orphan: %v", err)
	}

	rec, err := s.ReconcileNonTerminal(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Orphaned != 1 || rec.Total() != 1 {
		t.Fatalf("reconcile = %+v, want Orphaned 1 only", rec)
	}

	got, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateInterrupted {
		t.Fatalf("orphan gated job is %s, want interrupted", got.State)
	}
	if got.State == store.StateQueued || got.State == store.StateRunning {
		t.Fatalf("orphan gated job was made runnable (%s): a destructive apply must never auto-release", got.State)
	}
}

// TestReconcileLeavesLiveOrphanGatedJobAlone pins the #4 race guard: a gated job
// whose creator is still alive may be mid-CreateApproval, so the reconcile must
// not race it to interrupted.
func TestReconcileLeavesLiveOrphanGatedJobAlone(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	// Owned by this live process (CreateGatedJob stamps our pid): the creator is
	// present, so the missing approval is presumed in flight.
	job := gatedJob(t, s, "live-orphan")

	rec, err := s.ReconcileNonTerminal(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Total() != 0 {
		t.Fatalf("reconcile = %+v, want nothing touched", rec)
	}

	got, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateAwaitingApproval {
		t.Fatalf("live orphan gated job is %s, want awaiting_approval", got.State)
	}
}

// TestReconcilePreservesInterruptedSweep pins the interrupted-sweep behaviour the
// reconcile subsumes: queued and running rows whose owner is gone become
// interrupted, and a live
// owner is left alone. A pending gate is not swept even with a dead owner — the
// operator can still decide it, and no worker can claim it meanwhile.
func TestReconcilePreservesInterruptedSweep(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	running, err := s.CreateJob(ctx, store.NewJob{Runbook: "orphan-running", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.TransitionJob(ctx, running.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("run: %v", err)
	}
	queued, err := s.CreateJob(ctx, store.NewJob{Runbook: "orphan-queued", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	for _, id := range []int64{running.ID, queued.ID} {
		if err := s.SetOwnerPIDForTest(id, deadPID(t)); err != nil {
			t.Fatalf("orphan %d: %v", id, err)
		}
	}

	// A pending gate whose creator died: still decidable, must not be swept.
	pending := gatedJob(t, s, "pending-gate")
	pa, err := s.CreateApproval(ctx, store.Approval{
		JobID: pending.ID, ExpiresAt: time.Now().Add(time.Hour), TokenHash: "hash"})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	_ = pa
	if err := s.SetOwnerPIDForTest(pending.ID, deadPID(t)); err != nil {
		t.Fatalf("orphan pending: %v", err)
	}

	// A live queued row: untouchable.
	live, err := s.CreateJob(ctx, store.NewJob{Runbook: "live", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	rec, err := s.ReconcileNonTerminal(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Interrupted != 2 || rec.Total() != 2 {
		t.Fatalf("reconcile = %+v, want Interrupted 2 only", rec)
	}

	for id, want := range map[int64]store.JobState{
		running.ID: store.StateInterrupted,
		queued.ID:  store.StateInterrupted,
		pending.ID: store.StateAwaitingApproval,
		live.ID:    store.StateQueued,
	} {
		got, err := s.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("get job %d: %v", id, err)
		}
		if got.State != want {
			t.Errorf("job %d is %s, want %s", id, got.State, want)
		}
	}
}

// TestReconcileIsIdempotent runs the pass twice: the second finds nothing to do,
// because every move is a guarded transition over a snapshot of non-terminal
// rows. Safe to run on every idle tick.
func TestReconcileIsIdempotent(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	approved := gatedJob(t, s, "approved")
	settledApproval(t, s, approved.ID, store.ApprovalApproved)
	orphan := gatedJob(t, s, "orphan")
	if err := s.SetOwnerPIDForTest(orphan.ID, deadPID(t)); err != nil {
		t.Fatalf("orphan: %v", err)
	}

	if _, err := s.ReconcileNonTerminal(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	rec, err := s.ReconcileNonTerminal(ctx)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if rec.Total() != 0 {
		t.Fatalf("second reconcile = %+v, want nothing to do", rec)
	}
}
