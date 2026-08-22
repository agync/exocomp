package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// seedPlanWithOutcome registers a workspace and a plan job carrying the outcome
// event the tofu driver records — the values-free resource deltas evalPlanPolicy
// reads. It is what the driver persists after parsing `tofu show -json`.
func seedPlanWithOutcome(t *testing.T, ws string, outcome executor.TofuPlanOutcome) (*store.Store, int64) {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: ws, TofuDir: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	job, err := st.CreateJob(ctx, store.NewJob{
		Runbook: workspaceRunbookPrefix + ws, Kind: "tofu", Trigger: "manual",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	raw, err := json.Marshal(map[string]executor.TofuPlanOutcome{"tofu": outcome})
	if err != nil {
		t.Fatalf("marshal outcome: %v", err)
	}
	if _, err := st.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "tofu plan outcome", RawJSON: string(raw),
	}); err != nil {
		t.Fatalf("append outcome event: %v", err)
	}
	return st, job.ID
}

const deleteRuleExpr2 = `plan.resource_changes.exists(c, c.change.actions.exists(a, a == "delete"))`

// TestEvalPlanPolicyDeniesDeleteOutcome walks the seam over the driver's own
// recorded outcome: a deny policy over resource_changes matches a destroy, and
// dropping the policy clears the verdict — so the deny came from the rule
// matching the delete, not from the machinery.
func TestEvalPlanPolicyDeniesDeleteOutcome(t *testing.T) {
	initForKit(t)
	st, jobID := seedPlanWithOutcome(t, "homelab", executor.TofuPlanOutcome{
		Operation: "plan", Changes: true,
		Pending: []executor.ResourceDelta{
			{Address: "local_file.keep", Actions: []string{"update"}},
			{Address: "local_file.gone", Actions: []string{"delete"}},
		},
	})
	ctx := context.Background()

	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-deletes", Workspace: "homelab", Expr: deleteRuleExpr2, Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("create policy: %v", err)
	}

	ws, _ := st.WorkspaceByName(ctx, "homelab")
	var log strings.Builder
	v, err := evalPlanPolicy(ctx, st, &log, ws, jobID)
	if err != nil {
		t.Fatalf("evalPlanPolicy: %v (%s)", err, log.String())
	}
	if !v.Deny() || v.Policy != "no-deletes" {
		t.Fatalf("verdict = %+v, want deny by no-deletes", v)
	}

	if err := st.DeletePlanPolicy(ctx, "no-deletes"); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	v, err = evalPlanPolicy(ctx, st, &log, ws, jobID)
	if err != nil {
		t.Fatalf("evalPlanPolicy (no policy): %v", err)
	}
	if v.Matched() {
		t.Fatalf("with no policy the verdict = %+v, want clean", v)
	}
}

// TestEvalPlanPolicyCreateOnlyIsClean is the counter-case: the same deny rule
// does not fire on a plan that only creates.
func TestEvalPlanPolicyCreateOnlyIsClean(t *testing.T) {
	initForKit(t)
	st, jobID := seedPlanWithOutcome(t, "homelab", executor.TofuPlanOutcome{
		Operation: "plan", Changes: true,
		Pending: []executor.ResourceDelta{{Address: "local_file.new", Actions: []string{"create"}}},
	})
	ctx := context.Background()
	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-deletes", Workspace: "homelab", Expr: deleteRuleExpr2, Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	ws, _ := st.WorkspaceByName(ctx, "homelab")
	v, err := evalPlanPolicy(ctx, st, &strings.Builder{}, ws, jobID)
	if err != nil {
		t.Fatalf("evalPlanPolicy: %v", err)
	}
	if v.Matched() {
		t.Fatalf("verdict = %+v, want clean for a create-only plan", v)
	}
}

// TestEvalPlanPolicyFailsClosedWithoutDetail proves the fail-closed gap is
// covered: a plan that reports changes but whose detail the driver could not
// render is sealed denied, not waved through because the delete list came back
// empty.
func TestEvalPlanPolicyFailsClosedWithoutDetail(t *testing.T) {
	initForKit(t)
	st, jobID := seedPlanWithOutcome(t, "homelab", executor.TofuPlanOutcome{
		Operation: "plan", Changes: true, // no Pending, no Drift: detail unavailable
	})
	ctx := context.Background()
	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-deletes", Workspace: "homelab", Expr: deleteRuleExpr2, Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	ws, _ := st.WorkspaceByName(ctx, "homelab")
	var log strings.Builder
	v, err := evalPlanPolicy(ctx, st, &log, ws, jobID)
	if err != nil {
		t.Fatalf("evalPlanPolicy: %v", err)
	}
	if !v.Deny() {
		t.Fatalf("verdict = %+v, want a fail-closed deny; log: %s", v, log.String())
	}
}

// TestEvalPlanPolicyNoPoliciesSkips proves a workspace with no policies reads
// clean without needing any outcome at all.
func TestEvalPlanPolicyNoPoliciesSkips(t *testing.T) {
	initForKit(t)
	st, jobID := seedPlanWithOutcome(t, "homelab", executor.TofuPlanOutcome{Operation: "plan", Changes: false})
	ctx := context.Background()
	ws, _ := st.WorkspaceByName(ctx, "homelab")
	v, err := evalPlanPolicy(ctx, st, &strings.Builder{}, ws, jobID)
	if err != nil {
		t.Fatalf("evalPlanPolicy: %v", err)
	}
	if v.Matched() {
		t.Fatalf("verdict = %+v, want clean with no policies", v)
	}
}
