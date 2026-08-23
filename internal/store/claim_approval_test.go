package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestClaimJobRefusesPendingApproval is the claim-path half of the gate: a
// queued job that still carries a pending approval must not be claimable, so a
// mis-release or a race cannot run an unapproved step however it reached queued.
// A single gated apply is held by its awaiting_approval state; this proves a job
// is held the same way at the claim even once queued.
//
// Mutation check: drop the `NOT EXISTS (... approval ... state = 'pending')`
// join from ClaimJob's UPDATE. The first ClaimJob then succeeds, and this test
// fails on "want ErrApprovalPending".
func TestClaimJobRefusesPendingApproval(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	// A plain queued job (not gated) that nonetheless has a pending approval —
	// the exact defeat: a job in queued with its approval undecided.
	job, err := st.CreateJob(ctx, store.NewJob{
		Runbook: "wf:step", Kind: "shell", Trigger: "workflow",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job.State != store.StateQueued {
		t.Fatalf("job is %s, want queued", job.State)
	}
	approval, err := st.CreateApproval(ctx, store.Approval{
		JobID: job.ID, ExpiresAt: time.Now().Add(time.Hour), TokenHash: "hash",
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}

	// Unsettled: the claim is refused, and named for the gate, not a held key.
	if err := st.ClaimJob(ctx, job.ID); !errors.Is(err, store.ErrApprovalPending) {
		t.Fatalf("claim of a job with a pending approval = %v, want ErrApprovalPending", err)
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateQueued {
		t.Fatalf("refused job is %s, want it left queued", got.State)
	}

	// Once the approval settles, the same job claims normally.
	if _, err := st.DecideApproval(ctx, approval.ID, store.ApprovalApproved, "test", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := st.ClaimJob(ctx, job.ID); err != nil {
		t.Fatalf("claim after approval: %v", err)
	}
	got, err = st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateRunning {
		t.Fatalf("approved job is %s, want running", got.State)
	}
}
