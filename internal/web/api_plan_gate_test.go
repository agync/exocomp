package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
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

// seedPlanSeq makes each seeded sealed plan a distinct artifact on disk, so the
// tray's sha256 dedupe (SPEC §2.2, CONSUMED) treats two seeds as two plans the
// way real seals are — the sha of the ciphertext on disk differs per seal.
var seedPlanSeq atomic.Int64

// seedPlanArtifact writes a plan job and its sealed artifact, without a policy
// verdict — the state that exists between the seal and the verdict record.
func (h *harness) seedPlanArtifact(t *testing.T, ws string) store.Artifact {
	return h.seedPlanArtifactSerial(t, ws, 0)
}

// seedPlanArtifactSerial is seedPlanArtifact with a real PlanSerial (the
// workspace serial the plan was made against), so a tray row can be stale.
func (h *harness) seedPlanArtifactSerial(t *testing.T, ws string, planSerial int64) store.Artifact {
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
		Path:   "/data/artifacts/tofu/" + ws + ".tfplan.age",
		SHA256: fmt.Sprintf("seed-%d", seedPlanSeq.Add(1)), Size: 1,
		PlanSerial: planSerial,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	return art
}

// seedPlanReRecord writes the apply's same-SHA re-record of a sealed plan under
// the given applying job — the artifact row the CLI apply writes to link a
// consumed plan (SPEC §2.2). It is the shape the tray's CONSUMED dedupe reads.
func (h *harness) seedPlanReRecord(t *testing.T, art store.Artifact, applyJob int64) store.Artifact {
	t.Helper()
	re, err := h.store.CreateArtifact(context.Background(), store.Artifact{
		JobID: applyJob, Kind: art.Kind, Path: art.Path, SHA256: art.SHA256, Size: art.Size,
	})
	if err != nil {
		t.Fatalf("CreateArtifact re-record: %v", err)
	}
	return re
}

// seedPlanOutcome appends a plan-outcome event onto the plan job's timeline,
// shaped exactly as the tofu driver records it (`{"tofu":{...}}`), so the
// tray's CHANGES cell reads the counters/drift a real seal would have recorded.
func (h *harness) seedPlanOutcome(t *testing.T, art store.Artifact, changes bool, pending, drift []executor.ResourceDelta) {
	t.Helper()
	payload, err := json.Marshal(map[string]executor.TofuPlanOutcome{
		"tofu": {Operation: "plan", Changes: changes, Drift: drift, Pending: pending},
	})
	if err != nil {
		t.Fatalf("marshal plan outcome: %v", err)
	}
	if _, err := h.store.AppendJobEvent(context.Background(), store.JobEvent{
		JobID: art.JobID, Level: store.LevelSystem, Msg: "tofu plan", RawJSON: string(payload),
	}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}
}

// seedPlanVerdict appends a policy verdict event onto the plan job's timeline,
// shaped exactly as service.RecordPlanVerdict records it (`{"plan_policy":{...}}`)
// keyed to the sealed artifact, so the tray's VERDICT cell reads it. action is
// store.PolicyDeny, store.PolicyRequireApproval, or "no_match".
func (h *harness) seedPlanVerdict(t *testing.T, art store.Artifact, action, policy string) {
	t.Helper()
	payload, err := json.Marshal(map[string]map[string]any{
		"plan_policy": {"action": action, "policy": policy, "artifact": art.ID},
	})
	if err != nil {
		t.Fatalf("marshal plan verdict: %v", err)
	}
	if _, err := h.store.AppendJobEvent(context.Background(), store.JobEvent{
		JobID: art.JobID, Level: store.LevelSystem, Msg: "plan policy", RawJSON: string(payload),
	}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}
}

// TestAPIFreshApplyRefusedUnderPolicy proves a fresh apply cannot bypass plan
// policy: omitting "plan" used to skip the policy machinery entirely, so a
// workspace whose only policy denies everything still enqueued
// `tofu apply -auto-approve`. A fresh apply on a workspace with enabled plan
// policies is now refused — plan-then-apply-by-id is the only path there,
// because that is the only one a policy can adjudicate.
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

// TestAPIApplyRefusesUnadjudicatedPlan proves an apply cannot slip past an
// unrecorded verdict: between the artifact row and the verdict event, an
// apply-by-id used to read "no verdict" as "no policy" and enqueue plainly
// against a workspace whose policy denies everything. A plan on a workspace
// with enabled policies and no recorded adjudication is now unconsumable —
// 409, nothing enqueued.
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
