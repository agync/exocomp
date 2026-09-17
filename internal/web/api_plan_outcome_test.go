package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// seedTofuJob creates a tofu-kind job and appends one plan-outcome event
// carrying rawJSON, the shape the tofu driver writes (internal/executor). It
// lands the job in awaiting_approval via CreateGatedJob so no harness worker
// runs it; a queued tofu job with empty argv would execute and append a driver
// event between a test's API read and its later CLI projection.
func seedTofuJob(t *testing.T, st *store.Store, rawJSON string) int64 {
	t.Helper()
	ctx := context.Background()
	job, err := st.CreateGatedJob(ctx, store.NewJob{
		Runbook: "workspace:web", Kind: "tofu", Trigger: "manual",
	})
	if err != nil {
		t.Fatalf("CreateGatedJob: %v", err)
	}
	if _, err := st.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "tofu plan outcome", RawJSON: rawJSON,
	}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}
	return job.ID
}

// TestAPIJobGetPlanChangesPresent: GET /api/v1/jobs/{id} on a plan job that
// found changes projects plan.changes:true through the real HTTP route — the
// seam a remote `workspace plan` reads to reproduce the local exit-2 verdict.
func TestAPIJobGetPlanChangesPresent(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	jobID := seedTofuJob(t, h.store, `{"tofu":{"operation":"plan","changes":true,"pending":[`+
		`{"address":"aws_instance.web","actions":["create"]}]}}`)

	code, _, body := bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(jobID), token)
	if code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", code, body)
	}
	if !strings.Contains(body, `"plan":{"changes":true,"add":1,"addresses":["aws_instance.web"]}`) {
		t.Fatalf("job get missing plan.changes_present detail: %s", body)
	}
}

// TestAPIJobGetPlanNoChanges: a no-change plan projects plan.changes:false.
func TestAPIJobGetPlanNoChanges(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	jobID := seedTofuJob(t, h.store, `{"tofu":{"operation":"plan","changes":false}}`)

	_, _, body := bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(jobID), token)
	if !strings.Contains(body, `"plan":{"changes":false}`) {
		t.Fatalf("no-change plan job get missing plan.changes:false: %s", body)
	}
}

// TestAPIJobGetNonPlanHasNoPlanField: a shell job (no tofu/ansible_check
// event) carries no plan field at all through the live route.
func TestAPIJobGetNonPlanHasNoPlanField(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")
	ctx := context.Background()

	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)

	_, _, body := bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(job.ID), token)
	if strings.Contains(body, `"plan"`) {
		t.Fatalf("non-plan job get must carry no plan field: %s", body)
	}
}

// TestAPIJobGetPlanOutcomeNoAttributeValues seeds a plan event whose raw_json
// carries a fake attribute value on a field the outcome structs do not
// declare, and asserts the live job.get route never reflects it back.
func TestAPIJobGetPlanOutcomeNoAttributeValues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	const secret = "sk-FAKE-SECRET-VALUE-MUST-NOT-LEAK"
	jobID := seedTofuJob(t, h.store, `{"tofu":{"operation":"plan","changes":true,"pending":[`+
		`{"address":"aws_db_instance.main","actions":["update"],"attr_keys":["password"],"password":"`+secret+`"}]}}`)

	_, _, body := bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(jobID), token)
	if strings.Contains(body, secret) {
		t.Fatalf("job get leaked an attribute value: %s", body)
	}
}

// TestAPIJobEventsCarryPlanOnTerminalEvent: the events route projects Plan on
// the one event carrying the outcome payload, matching wire.NewJobEventList.
func TestAPIJobEventsCarryPlanOnTerminalEvent(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	jobID := seedTofuJob(t, h.store, `{"tofu":{"operation":"plan","changes":true}}`)

	_, _, body := bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(jobID)+"/events", token)
	if !strings.Contains(body, `"plan":{"changes":true}`) {
		t.Fatalf("job events missing plan on terminal event: %s", body)
	}

	want, err := wire.MarshalSuccess(wire.KindJobEvents, wire.NewJobEventList(mustListEvents(t, h.store, jobID)))
	if err != nil {
		t.Fatalf("MarshalSuccess: %v", err)
	}
	if body != string(want) {
		t.Fatalf("events bytes differ from CLI projection:\n api: %q\n cli: %q", body, want)
	}
}

func mustListEvents(t *testing.T, st *store.Store, jobID int64) []store.JobEvent {
	t.Helper()
	rows, err := st.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	return rows
}
