package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// decodeData unmarshals a wire success envelope's data into v.
func decodeData(t *testing.T, body string, v any) {
	t.Helper()
	_, _, data := decodeEnvelope(t, body)
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode envelope data: %v (%s)", err, body)
	}
}

// TestAPIDecisionRefusesOperatorScope proves the scope that may enqueue a
// write cannot also settle the approval that write raised. On the machine
// API, A is the approve scope alone (Decision 0014 §3's R/W/A mapping), so an
// operator token — the only scope that can plan/apply — is forbidden, and the
// gated job stays parked.
func TestAPIDecisionRefusesOperatorScope(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	job, approval := h.gatedJob(t, time.Hour)
	op := h.mint(t, "op", "operator")
	path := "/api/v1/approvals/" + itoa64(approval.ID) + "/decision"

	code, _, body := bearerPost(t, ts.URL, path, op, `{"decision":"approve"}`, nil)
	if code != http.StatusForbidden {
		t.Fatalf("operator decision status = %d, want 403 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "forbidden" {
		t.Fatalf("operator decision envelope = %s, want error/forbidden", body)
	}
	parked, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if parked.State != store.StateAwaitingApproval {
		t.Fatalf("job state = %q, want awaiting_approval — the write scope released the gate", parked.State)
	}

	// The approve scope — which cannot enqueue anything — still decides.
	ap := h.mint(t, "approver", "approve")
	code, _, body = bearerPost(t, ts.URL, path, ap, `{"decision":"approve"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("approve-scope decision status = %d, want 200 (%s)", code, body)
	}
	h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)
}

// TestDecisionRefusesSelfApproval proves the same self-approval refusal on
// the browser surface: whatever the scopes say, the principal recorded on a
// gated job's ledger as having asked for it may not settle its own approval.
// The browser surface is the reachable case — an operator token enqueues a
// gated apply over the API, then decides it from its own session — and the
// refusal holds there.
func TestDecisionRefusesSelfApproval(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	h.seedWorkspace(t, "web")

	// A sealed plan carrying a require_approval verdict, the shape a plan policy
	// leaves behind.
	planJob, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook("web"), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue plan job: %v", err)
	}
	art, err := h.store.CreateArtifact(ctx, store.Artifact{
		JobID: planJob.ID, Kind: reactive.PlanArtifactKind, Path: "/dev/null", SHA256: "x", Size: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	raw := fmt.Sprintf(`{"plan_policy":{"action":"require_approval","policy":"prod-guard","artifact":%d}}`, art.ID)
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: planJob.ID, Level: store.LevelInfo, Msg: "policy", RawJSON: raw,
	}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}

	op := h.mint(t, "agent", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/apply", op,
		fmt.Sprintf(`{"plan":"%d"}`, art.ID), nil)
	if code != http.StatusOK {
		t.Fatalf("gated apply status = %d, want 200 (%s)", code, body)
	}
	var av approvalPendingView
	decodeData(t, body, &av)

	// The same token, now in a browser session, tries to release its own gate.
	client := login(t, ts.URL, op)
	resp, err := client.PostForm(ts.URL+"/approvals/"+itoa64(av.ApprovalID)+"/decide",
		url.Values{"decision": {"approve"}})
	if err != nil {
		t.Fatalf("POST decide: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("self-approval status = %d, want 403", resp.StatusCode)
	}
	parked, err := h.store.GetJob(ctx, av.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if parked.State != store.StateAwaitingApproval {
		t.Fatalf("job state = %q, want awaiting_approval — a token released its own gate", parked.State)
	}

	// A different principal still decides it: the guard is identity, not a block
	// on the gate itself.
	other := h.mint(t, "human", "approve")
	otherClient := login(t, ts.URL, other)
	resp, err = otherClient.PostForm(ts.URL+"/approvals/"+itoa64(av.ApprovalID)+"/decide",
		url.Values{"decision": {"approve"}})
	if err != nil {
		t.Fatalf("POST decide (other): %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second-principal decide status = %d, want 200", resp.StatusCode)
	}
	settled, err := h.store.GetApproval(ctx, av.ApprovalID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if settled.State != store.ApprovalApproved {
		t.Fatalf("approval state = %q, want approved", settled.State)
	}
}
