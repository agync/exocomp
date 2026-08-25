package store

import (
	"context"
	"testing"
	"time"
)

// TestApprovalStalenessReportsMovement: a workspace that advanced past the
// plan's serial is stale, and the movement names the mover job, its trigger,
// and when — sourced from the state ledger, not a rendered sentence.
func TestApprovalStalenessReportsMovement(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	ws, err := st.CreateWorkspace(ctx, Workspace{Name: "prod", TofuDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	gated, err := st.CreateJob(ctx, NewJob{Runbook: "workspace:prod", Kind: "tofu", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create gated job: %v", err)
	}
	a, err := st.CreateApproval(ctx, Approval{
		JobID: gated.ID, Workspace: "prod", PlanSerial: 0,
		RequestedAt: now, ExpiresAt: now.Add(time.Hour), TokenHash: "h",
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}

	// Still at serial 0: not stale.
	if _, stale, err := st.ApprovalStaleness(ctx, a); err != nil || stale {
		t.Fatalf("fresh staleness = (stale=%v, err=%v), want not stale", stale, err)
	}

	mover, err := st.CreateJob(ctx, NewJob{Runbook: "workspace:prod", Kind: "tofu", Trigger: "schedule"})
	if err != nil {
		t.Fatalf("create mover: %v", err)
	}
	if _, err := st.AppendTFState(ctx, NewTFState{
		WorkspaceID: ws.ID, SerialHint: 7, JobID: &mover.ID, Ciphertext: []byte("cipher"),
	}); err != nil {
		t.Fatalf("append tfstate: %v", err)
	}

	move, stale, err := st.ApprovalStaleness(ctx, a)
	if err != nil || !stale {
		t.Fatalf("moved staleness = (stale=%v, err=%v), want stale", stale, err)
	}
	if move.PlanSerial != 0 || move.CurrentSerial != 7 {
		t.Fatalf("movement serials = %d -> %d, want 0 -> 7", move.PlanSerial, move.CurrentSerial)
	}
	if move.MovedByJob == nil || *move.MovedByJob != mover.ID {
		t.Fatalf("moved_by_job = %v, want %d", move.MovedByJob, mover.ID)
	}
	if move.MovedVia != "schedule" {
		t.Fatalf("moved_via = %q, want schedule", move.MovedVia)
	}
	if move.MovedAt.IsZero() {
		t.Fatalf("moved_at is zero, want the mover version's timestamp")
	}
}

// TestApprovalStalenessNoWorkspace: an approval with no workspace (a
// non-workspace gated action) is never stale.
func TestApprovalStalenessNoWorkspace(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a := Approval{JobID: 1, RequestedAt: now, ExpiresAt: now.Add(time.Hour)}
	if _, stale, err := st.ApprovalStaleness(ctx, a); err != nil || stale {
		t.Fatalf("no-workspace staleness = (stale=%v, err=%v), want not stale", stale, err)
	}
}
