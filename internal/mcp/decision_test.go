package mcp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// seedGatedApproval writes a job straight into awaiting_approval with a pending
// approval bound to it — the shape a require_approval plan policy or a reactive
// gate produces, and the state finding AI-1 attacked.
func seedGatedApproval(t *testing.T, st *store.Store) (store.Job, store.Approval) {
	t.Helper()
	ctx := context.Background()
	job, err := st.CreateGatedJob(ctx, store.NewJob{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/sh", "-c", "echo DESTRUCTIVE-ACTION-RAN"}},
	})
	if err != nil {
		t.Fatalf("CreateGatedJob: %v", err)
	}
	now := time.Now().UTC()
	approval, err := st.CreateApproval(ctx, store.Approval{
		JobID: job.ID, TokenHash: "seeded", RequestedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	return job, approval
}

// TestDecisionToolsAbsentFromStdioFace is the AI-1 regression: the stdio face
// carries no credential and no scope, so it must not carry the approval gate's
// release either. approve_job/reject_job are not advertised and not callable;
// the gated job stays in awaiting_approval and its approval stays pending.
func TestDecisionToolsAbsentFromStdioFace(t *testing.T) {
	srv, st := newTestServer(t)
	job, approval := seedGatedApproval(t, st)

	resps := drive(t, srv,
		req(1, "tools/list", "{}"),
		req(2, "tools/call", `{"name":"approve_job","arguments":{"approval_id":1}}`),
		req(3, "tools/call", `{"name":"reject_job","arguments":{"approval_id":1}}`),
	)
	if len(resps) != 3 {
		t.Fatalf("want 3 responses, got %d", len(resps))
	}

	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resps[0].Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	for _, tl := range list.Tools {
		if tl.Name == "approve_job" || tl.Name == "reject_job" {
			t.Errorf("catalog advertises %q on the unauthenticated stdio face", tl.Name)
		}
	}

	for i, name := range []string{"approve_job", "reject_job"} {
		r := resps[i+1]
		if r.Error == nil {
			t.Errorf("%s was dispatched on the stdio face: %s", name, r.Result)
		}
	}

	ctx := context.Background()
	after, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if after.State != store.StateAwaitingApproval {
		t.Fatalf("gated job state = %q, want awaiting_approval — the gate was released from stdio", after.State)
	}
	settled, err := st.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if settled.State != store.ApprovalPending {
		t.Fatalf("approval state = %q, want pending", settled.State)
	}
}
