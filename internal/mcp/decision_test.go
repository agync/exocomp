package mcp_test

import (
	"context"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/mcp"
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

// TestOperatorCannotReleaseTheGateItRaises is the AI-1 regression: the scope
// that can raise the approval gate with workspace_apply (operator) must not
// also hold its release. approve_job/reject_job are not advertised to an
// operator token and not callable by one; the gated job stays in
// awaiting_approval and its approval stays pending.
func TestOperatorCannotReleaseTheGateItRaises(t *testing.T) {
	srv, st := newTestServer(t)
	job, approval := seedGatedApproval(t, st)
	op := mcp.TokenCaller(store.ScopeOperator, "op", 1, nil)

	list := call(t, srv, op, req(1, "tools/list", "{}"))
	approveResp := call(t, srv, op, req(2, "tools/call", `{"name":"approve_job","arguments":{"approval_id":1}}`))
	rejectResp := call(t, srv, op, req(3, "tools/call", `{"name":"reject_job","arguments":{"approval_id":1}}`))

	tools := toolNames(t, list.Result)
	for _, name := range []string{"approve_job", "reject_job"} {
		if tools[name] {
			t.Errorf("catalog advertises %q to an operator-scope caller", name)
		}
	}

	for _, r := range []rpcResp{approveResp, rejectResp} {
		if r.Error != nil {
			continue // refused at the protocol level
		}
		env, isErr := toolContent(t, r.Result)
		if !isErr || env.Error == nil || env.Error.Code != "forbidden" {
			t.Errorf("a decision tool was dispatched for an operator caller: %s", r.Result)
		}
	}

	ctx := context.Background()
	after, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if after.State != store.StateAwaitingApproval {
		t.Fatalf("gated job state = %q, want awaiting_approval — the gate was released by operator scope", after.State)
	}
	settled, err := st.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if settled.State != store.ApprovalPending {
		t.Fatalf("approval state = %q, want pending", settled.State)
	}
}
