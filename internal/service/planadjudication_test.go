package service_test

import (
	"context"
	"errors"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// TestPlanGateForPlanFailsClosed: on a workspace carrying enabled policies, a
// sealed plan with no adjudication record — the state between CreateArtifact and
// the verdict event — is unconsumable rather than unpoliced, and
// the refusal reads as a conflict through the shared taxonomy.
func TestPlanGateForPlanFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	svc := service.New(st, t.TempDir(), nil)
	jobID := seedPlanJob(t, st, "prod", executor.TofuPlanOutcome{
		Operation: "plan", Changes: true,
		Pending: []executor.ResourceDelta{{Address: "local_file.gone", Actions: []string{"delete"}}},
	})
	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-deletes", Workspace: "prod", Expr: deleteRule, Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("CreatePlanPolicy: %v", err)
	}
	art, err := st.CreateArtifact(ctx, store.Artifact{
		JobID: jobID, Kind: "tofu-plan", Path: "/x.age", SHA256: "s", Size: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	_, err = svc.PlanGateForPlan(ctx, "prod", art)
	var unadjudicated *service.PlanUnadjudicatedError
	if !errors.As(err, &unadjudicated) {
		t.Fatalf("PlanGateForPlan on an undecided plan = %v, want PlanUnadjudicatedError", err)
	}
	if !errors.Is(err, store.ErrStateConflict) {
		t.Errorf("the refusal does not read as a conflict: %v", err)
	}

	// Recording the verdict makes the plan decidable again — as a deny.
	res, err := svc.EvalPlanPolicy(ctx, mustWorkspace(t, st, "prod"), jobID)
	if err != nil {
		t.Fatalf("EvalPlanPolicy: %v", err)
	}
	if _, err := svc.RecordPlanVerdict(ctx, jobID, art.ID, res.Verdict); err != nil {
		t.Fatalf("RecordPlanVerdict: %v", err)
	}
	gate, err := svc.PlanGateForPlan(ctx, "prod", art)
	if err != nil {
		t.Fatalf("PlanGateForPlan after the verdict: %v", err)
	}
	if !gate.Deny {
		t.Fatalf("gate = %+v, want Deny", gate)
	}
}

// TestPlanGateForPlanAdmitsUnmatchedPlan: the fail-closed rule must not refuse a
// plan the policies legitimately did not match. The adjudication record is what
// separates "policies ran, nothing matched" from "the verdict has not landed".
func TestPlanGateForPlanAdmitsUnmatchedPlan(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	svc := service.New(st, t.TempDir(), nil)
	jobID := seedPlanJob(t, st, "prod", executor.TofuPlanOutcome{
		Operation: "plan", Changes: true,
		Pending: []executor.ResourceDelta{{Address: "local_file.new", Actions: []string{"create"}}},
	})
	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-deletes", Workspace: "prod", Expr: deleteRule, Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("CreatePlanPolicy: %v", err)
	}
	art, err := st.CreateArtifact(ctx, store.Artifact{
		JobID: jobID, Kind: "tofu-plan", Path: "/x.age", SHA256: "s", Size: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	res, err := svc.EvalPlanPolicy(ctx, mustWorkspace(t, st, "prod"), jobID)
	if err != nil {
		t.Fatalf("EvalPlanPolicy: %v", err)
	}
	if res.Verdict.Matched() || !res.Evaluated {
		t.Fatalf("result = %+v, want an evaluated non-match", res)
	}
	if err := svc.RecordPlanAdjudication(ctx, jobID, art.ID); err != nil {
		t.Fatalf("RecordPlanAdjudication: %v", err)
	}

	gate, err := svc.PlanGateForPlan(ctx, "prod", art)
	if err != nil {
		t.Fatalf("PlanGateForPlan on an adjudicated non-match: %v", err)
	}
	if gate.Deny || gate.RequireApproval {
		t.Fatalf("gate = %+v, want a plain apply", gate)
	}
	// The adjudication record is not a verdict: every existing reader still sees
	// a clean plan.
	v, err := svc.PlanVerdict(ctx, art)
	if err != nil || v.Matched() {
		t.Fatalf("PlanVerdict = %+v, %v — the adjudication leaked into the verdict", v, err)
	}
}

func mustWorkspace(t *testing.T, st *store.Store, name string) store.Workspace {
	t.Helper()
	ws, err := st.WorkspaceByName(context.Background(), name)
	if err != nil {
		t.Fatalf("WorkspaceByName: %v", err)
	}
	return ws
}
