package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// gatedWorkspaceApproval writes a gated job bound to a workspace and the
// approval that releases it, the shape a gated tofu apply produces. It returns
// the job and the approval pinned to the workspace at planSerial.
func (h *harness) gatedWorkspaceApproval(t *testing.T, ws string, planSerial int64) (store.Job, store.Approval) {
	t.Helper()
	ctx := context.Background()
	job, err := h.queue.EnqueueGated(ctx, queue.Spec{
		Runbook: "workspace:" + ws, Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	_, hash, err := reactive.ApprovalToken(h.env, job.ID)
	if err != nil {
		t.Fatalf("ApprovalToken: %v", err)
	}
	now := time.Now().UTC()
	a, err := h.store.CreateApproval(ctx, store.Approval{
		JobID: job.ID, Workspace: ws, PlanSerial: planSerial,
		TokenHash: hash, RequestedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	return job, a
}

// TestAPIApprovalGetStaleAndProvenance: approval.get carries no stale block
// while the workspace is unmoved, a manual-origin provenance when no alert
// raised it, an alert-origin provenance once an event names the job, and a
// structured stale block once the workspace advances past the plan's serial.
func TestAPIApprovalGetStaleAndProvenance(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	ws, err := h.store.CreateWorkspace(ctx, store.Workspace{Name: "prod", TofuDir: t.TempDir()})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	job, a := h.gatedWorkspaceApproval(t, "prod", 0)
	ro := h.mint(t, "phone", "readonly")
	path := "/api/v1/approvals/" + itoa64(a.ID)

	// Fresh: no stale block; origin is manual (no alert raised this job).
	v := getApproval(t, ts.URL, path, ro)
	if v.Stale != nil {
		t.Fatalf("fresh approval carries a stale block: %+v", v.Stale)
	}
	if v.Provenance == nil || v.Provenance.Origin != wire.OriginManual {
		t.Fatalf("fresh provenance = %+v, want origin manual", v.Provenance)
	}
	if v.Provenance.Alert != "" || v.Provenance.Rule != "" {
		t.Fatalf("manual provenance carries alert/rule: %+v", v.Provenance)
	}

	// An event that named the job makes the origin an alert, with the alert name
	// from the event's structured normalized_json and the rule from its column.
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", MatchedRule: "apply-on-drift", Outcome: store.OutcomeGated,
		Normalized: `{"name":"DiskFull"}`, JobID: &job.ID, ReceivedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	v = getApproval(t, ts.URL, path, ro)
	if v.Provenance == nil || v.Provenance.Origin != wire.OriginAlert {
		t.Fatalf("provenance = %+v, want origin alert", v.Provenance)
	}
	if v.Provenance.Alert != "DiskFull" || v.Provenance.Rule != "apply-on-drift" || v.Provenance.MatchedAt == nil {
		t.Fatalf("alert provenance = %+v, want DiskFull/apply-on-drift/matched_at", v.Provenance)
	}

	// The workspace advances past the plan's serial: the stale block appears.
	mover, err := h.store.CreateJob(ctx, store.NewJob{Runbook: "workspace:prod", Kind: "tofu", Trigger: "manual"})
	if err != nil {
		t.Fatalf("CreateJob mover: %v", err)
	}
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 5, JobID: &mover.ID, Ciphertext: []byte("cipher"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}
	v = getApproval(t, ts.URL, path, ro)
	if v.Stale == nil {
		t.Fatalf("moved approval carries no stale block (%+v)", v)
	}
	if v.Stale.CurrentSerial != 5 || v.Stale.PlanSerial != 0 {
		t.Fatalf("stale serials = %d -> %d, want 0 -> 5", v.Stale.PlanSerial, v.Stale.CurrentSerial)
	}
	if v.Stale.MovedByJob == nil || *v.Stale.MovedByJob != mover.ID || v.Stale.MovedVia != "manual" {
		t.Fatalf("stale mover = %+v, want job %d via manual", v.Stale, mover.ID)
	}
}

// TestAPIDecisionStaleBodyCarriesStale: the 409 stale_plan decision refusal
// carries the same structured stale block the approval.get view does, so a
// client reads why the plan cannot apply from fields, not the message.
func TestAPIDecisionStaleBodyCarriesStale(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	ws, err := h.store.CreateWorkspace(ctx, store.Workspace{Name: "prod", TofuDir: t.TempDir()})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	_, a := h.gatedWorkspaceApproval(t, "prod", 0)

	// Move the workspace so approving the saved plan is refused.
	mover, err := h.store.CreateJob(ctx, store.NewJob{Runbook: "workspace:prod", Kind: "tofu", Trigger: "schedule"})
	if err != nil {
		t.Fatalf("CreateJob mover: %v", err)
	}
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 9, JobID: &mover.ID, Ciphertext: []byte("cipher"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}

	approve := h.mint(t, "approver", "approve")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/approvals/"+itoa64(a.ID)+"/decision",
		approve, `{"decision":"approve"}`, nil)
	if code != http.StatusConflict {
		t.Fatalf("stale decision status = %d, want 409 (%s)", code, body)
	}
	var env struct {
		Error struct {
			Code  string                  `json:"code"`
			Stale *wire.ApprovalStaleView `json:"stale"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, body)
	}
	if env.Error.Code != wire.CodeStalePlan {
		t.Fatalf("error code = %q, want %q", env.Error.Code, wire.CodeStalePlan)
	}
	if env.Error.Stale == nil || env.Error.Stale.CurrentSerial != 9 {
		t.Fatalf("409 body stale block = %+v, want current_serial 9", env.Error.Stale)
	}
	if env.Error.Stale.MovedVia != "schedule" {
		t.Fatalf("409 body moved_via = %q, want schedule", env.Error.Stale.MovedVia)
	}
}

// getApproval fetches one approval.get and decodes its view.
func getApproval(t *testing.T, base, path, token string) wire.ApprovalView {
	t.Helper()
	code, _, body := bearerGet(t, base, path, token)
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 (%s)", path, code, body)
	}
	var v wire.ApprovalView
	decodeData(t, body, &v)
	return v
}
