package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// actorEventPayload mirrors the service package's unexported payload shape
// for decoding a recorded actor event's RawJSON in tests.
type actorEventPayload struct {
	ActorKind string `json:"actor_kind"`
	ActorName string `json:"actor_name"`
	Face      string `json:"face"`
}

// findActorEvent returns the first job event whose RawJSON decodes to an
// actor payload, and whether one was found.
func findActorEvent(t *testing.T, h *harness, jobID int64) (store.JobEvent, actorEventPayload, bool) {
	t.Helper()
	events, err := h.store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%d): %v", jobID, err)
	}
	for _, ev := range events {
		var payload actorEventPayload
		if json.Unmarshal([]byte(ev.RawJSON), &payload) == nil && payload.ActorKind != "" {
			return ev, payload, true
		}
	}
	return store.JobEvent{}, actorEventPayload{}, false
}

// TestAPIPlanRecordsActorOnJobEvents is the GATE: an API plan POST with an
// operator token produces a job whose event ledger carries the actor record
// (token name, face api), and the record never contains the bearer secret.
func TestAPIPlanRecordsActorOnJobEvents(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "web")

	jobID := postPlanJobID(t, ts.URL, token, nil)

	ev, payload, found := findActorEvent(t, h, jobID)
	if !found {
		t.Fatalf("job %d has no actor event", jobID)
	}
	if ev.Level != store.LevelSystem {
		t.Errorf("actor event level = %q, want %q", ev.Level, store.LevelSystem)
	}
	if payload.ActorKind != "token" || payload.ActorName != "op" || payload.Face != "api" {
		t.Errorf("actor payload = %+v, want kind=token name=op face=api", payload)
	}
	if strings.Contains(ev.Msg, token) || strings.Contains(ev.RawJSON, token) {
		t.Fatal("actor record contains the bearer token secret")
	}
}

// TestAPIApplyRecordsActorOnJobEvents covers the fresh (non-gated) apply path.
func TestAPIApplyRecordsActorOnJobEvents(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "web")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/apply", token, "", nil)
	if code != 200 {
		t.Fatalf("apply status = %d (%s)", code, body)
	}
	_, _, data := decodeEnvelope(t, body)
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil {
		t.Fatalf("apply data: %v", err)
	}

	_, payload, found := findActorEvent(t, h, jv.JobID)
	if !found {
		t.Fatalf("job %d has no actor event", jv.JobID)
	}
	if payload.ActorName != "op" {
		t.Errorf("actor name = %q, want op", payload.ActorName)
	}
}

// TestAPIApplyGatedRecordsRequestingActor is the second GATE: a gated apply's
// pending approval records the requesting actor — read back through the gated
// job's own event ledger, the same one the approval's JobID points at.
func TestAPIApplyGatedRecordsRequestingActor(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	ctx := context.Background()
	h.seedWorkspace(t, "web")

	// A prior plan job, its sealed plan artifact, and the values-free policy
	// verdict recorded on that job's timeline — the shape `workspace plan`
	// leaves (mirrors api_write_test.go's TestAPIApplyGatedReturnsPending).
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

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/apply", token,
		fmt.Sprintf(`{"plan":"%d"}`, art.ID), nil)
	if code != 200 {
		t.Fatalf("gated apply status = %d (%s)", code, body)
	}
	kind, _, data := decodeEnvelope(t, body)
	if kind != kindApprovalPending {
		t.Fatalf("kind = %q, want %q", kind, kindApprovalPending)
	}
	var av approvalPendingView
	if err := json.Unmarshal(data, &av); err != nil {
		t.Fatalf("pending data: %v", err)
	}

	_, payload, found := findActorEvent(t, h, av.JobID)
	if !found {
		t.Fatalf("gated job %d has no actor event", av.JobID)
	}
	if payload.ActorName != "op" || payload.Face != "api" {
		t.Errorf("actor payload = %+v, want name=op face=api", payload)
	}
}

// TestAPIPlaybookRunRecordsActorOnJobEvents covers the ansible playbook write.
func TestAPIPlaybookRunRecordsActorOnJobEvents(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	jobID := postRunJobID(t, ts.URL, token, nil)

	_, payload, found := findActorEvent(t, h, jobID)
	if !found {
		t.Fatalf("job %d has no actor event", jobID)
	}
	if payload.ActorName != "op" {
		t.Errorf("actor name = %q, want op", payload.ActorName)
	}
}
