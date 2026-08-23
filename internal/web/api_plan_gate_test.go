package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// seedPlanPolicy registers one enabled plan policy on a workspace.
func (h *harness) seedPlanPolicy(t *testing.T, ws, name, action string) {
	t.Helper()
	_, err := h.store.CreatePlanPolicy(context.Background(), store.PlanPolicy{
		Name: name, Workspace: ws, Expr: "plan.changes", Action: action, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreatePlanPolicy: %v", err)
	}
}

// seedPlanArtifact writes a plan job and its sealed artifact, without a policy
// verdict — the state that exists between the seal and the verdict record.
func (h *harness) seedPlanArtifact(t *testing.T, ws string) store.Artifact {
	t.Helper()
	ctx := context.Background()
	planJob, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook(ws), Kind: "tofu", Trigger: service.TriggerAPI,
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue plan job: %v", err)
	}
	art, err := h.store.CreateArtifact(ctx, store.Artifact{
		JobID: planJob.ID, Kind: reactive.PlanArtifactKind,
		Path: "/data/artifacts/tofu/" + ws + ".tfplan.age", SHA256: "x", Size: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	return art
}

// TestAPIFreshApplyRefusedUnderPolicy is the AI-4 regression: omitting "plan"
// used to skip the policy machinery entirely, so a workspace whose only policy
// denies everything still enqueued `tofu apply -auto-approve`. A fresh apply on
// a workspace with enabled plan policies is now refused — plan-then-apply-by-id
// is the only path there, because that is the only one a policy can adjudicate.
func TestAPIFreshApplyRefusedUnderPolicy(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "prod")
	h.seedPlanPolicy(t, "prod", "no-destroy", store.PolicyDeny)

	for _, body := range []string{"", "{}"} {
		code, _, resp := bearerPost(t, ts.URL, "/api/v1/workspaces/prod/apply", token, body, nil)
		if code != http.StatusConflict {
			t.Fatalf("fresh apply body %q status = %d, want 409 (%s)", body, code, resp)
		}
		if kind, ecode, _ := decodeEnvelope(t, resp); kind != "error" || ecode != "conflict" {
			t.Fatalf("fresh apply envelope = %s, want error/conflict", resp)
		}
	}
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{Runbook: service.WorkspaceRunbook("prod")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused fresh apply still enqueued %d job(s)", len(jobs))
	}

	// A workspace with no policies is untouched: the fresh apply still enqueues.
	h.seedWorkspace(t, "lab")
	code, _, resp := bearerPost(t, ts.URL, "/api/v1/workspaces/lab/apply", token, "", nil)
	if code != http.StatusOK {
		t.Fatalf("unpolicied fresh apply status = %d, want 200 (%s)", code, resp)
	}
}

// TestAPIApplyRefusesUnadjudicatedPlan is the AI-3 regression: between the
// artifact row and the verdict event, an apply-by-id used to read "no verdict"
// as "no policy" and enqueue plainly against a workspace whose policy denies
// everything. A plan on a workspace with enabled policies and no recorded
// adjudication is now unconsumable — 409, nothing enqueued.
func TestAPIApplyRefusesUnadjudicatedPlan(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "prod")
	h.seedPlanPolicy(t, "prod", "no-destroy", store.PolicyDeny)
	art := h.seedPlanArtifact(t, "prod")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/prod/apply", token,
		fmt.Sprintf(`{"plan":"%d"}`, art.ID), nil)
	if code != http.StatusConflict {
		t.Fatalf("unadjudicated apply status = %d, want 409 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "conflict" {
		t.Fatalf("unadjudicated apply envelope = %s, want error/conflict", body)
	}
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{Runbook: service.WorkspaceRunbook("prod")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("workspace holds %d jobs, want only the plan job — an apply slipped past the gate", len(jobs))
	}

	// Control: once the plan IS adjudicated, the recorded verdict routes it. A
	// require_approval verdict lands the apply behind an approval, not plainly.
	raw := fmt.Sprintf(`{"plan_policy":{"action":"require_approval","policy":"no-destroy","artifact":%d}}`, art.ID)
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: art.JobID, Level: store.LevelSystem, Msg: "policy", RawJSON: raw,
	}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}
	code, _, body = bearerPost(t, ts.URL, "/api/v1/workspaces/prod/apply", token,
		fmt.Sprintf(`{"plan":"%d"}`, art.ID), nil)
	if code != http.StatusOK {
		t.Fatalf("adjudicated apply status = %d, want 200 (%s)", code, body)
	}
	if kind, _, _ := decodeEnvelope(t, body); kind != kindApprovalPending {
		t.Fatalf("adjudicated apply kind = %q, want %q (%s)", kind, kindApprovalPending, body)
	}
}
