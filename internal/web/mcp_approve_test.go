package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// TestMCPScopesStayDisjoint is the AI-1/AI-2 regression on the MCP HTTP face: a
// token is advertised the write column or the approve column, never both, and
// the per-call check refuses the other one anyway. An agent that can raise the
// approval gate therefore cannot hold its release, and vice versa.
func TestMCPScopesStayDisjoint(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	h.seedWorkspace(t, "web")
	job, approval := h.gatedJob(t, time.Hour)

	approver := h.mint(t, "approver", "approve")
	operator := h.mint(t, "op", "operator")
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

	a := mcpToolNames(t, rpcOK(t, ts.URL, approver, list).Result)
	if !a["approve_job"] || !a["reject_job"] {
		t.Fatalf("approve token is not advertised the decision tools: %v", a)
	}
	for _, w := range []string{"workspace_plan", "workspace_apply"} {
		if a[w] {
			t.Errorf("approve token is advertised the write tool %q", w)
		}
	}

	o := mcpToolNames(t, rpcOK(t, ts.URL, operator, list).Result)
	for _, d := range []string{"approve_job", "reject_job"} {
		if o[d] {
			t.Errorf("operator token is advertised the decision tool %q", d)
		}
	}

	// The operator token calls the decision tool anyway: refused, gate closed.
	call := rpcOK(t, ts.URL, operator, rpcCall("approve_job", `{"approval_id":`+itoa64(approval.ID)+`}`))
	if _, code, _, isErr := mcpToolEnvelope(t, call.Result); !isErr || code != "forbidden" {
		t.Fatalf("operator approve_job = code %q isError %v, want forbidden", code, isErr)
	}

	// The approve token calls a write tool anyway: refused, nothing enqueued.
	plan := rpcOK(t, ts.URL, approver, rpcCall("workspace_plan", `{"name":"web"}`))
	if _, code, _, isErr := mcpToolEnvelope(t, plan.Result); !isErr || code != "forbidden" {
		t.Fatalf("approve workspace_plan = code %q isError %v, want forbidden", code, isErr)
	}
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{Runbook: "workspace:web"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("an approve-scope token enqueued %d job(s)", len(jobs))
	}

	parked, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if parked.State != store.StateAwaitingApproval {
		t.Fatalf("gated job state = %q, want awaiting_approval", parked.State)
	}
	settled, err := h.store.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if settled.State != store.ApprovalPending {
		t.Fatalf("approval state = %q, want pending", settled.State)
	}
}

// TestMCPApproveToolReleasesGate asserts the A column actually works for the one
// scope that holds it: an approve token settles the approval through the same
// reactive path every other surface decides on, and the job leaves the gate.
func TestMCPApproveToolReleasesGate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job, approval := h.gatedJob(t, time.Hour)
	approver := h.mint(t, "approver", "approve")

	call := rpcOK(t, ts.URL, approver, rpcCall("approve_job", `{"approval_id":`+itoa64(approval.ID)+`}`))
	kind, code, data, isErr := mcpToolEnvelope(t, call.Result)
	if isErr {
		t.Fatalf("approve_job failed: code %q", code)
	}
	if kind != "approval.decision" {
		t.Fatalf("kind = %q, want approval.decision", kind)
	}
	var view struct {
		ApprovalID int64  `json:"approval_id"`
		Outcome    string `json:"outcome"`
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatalf("decode decision: %v (%s)", err, data)
	}
	if view.ApprovalID != approval.ID || view.Outcome == "" {
		t.Fatalf("decision view = %+v", view)
	}

	settled, err := h.store.GetApproval(context.Background(), approval.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if settled.State == store.ApprovalPending {
		t.Fatalf("approval is still pending after an approve-scope decision")
	}
	if job.State != store.StateAwaitingApproval {
		t.Fatalf("precondition: gated job was %q", job.State)
	}
}

// TestMCPLeaksNoSecretOrCapability seeds sentinels a response must never carry —
// a secret's ciphertext and nonce, and the job's real approval capability — and
// drives the whole readable surface plus a decision over MCP. A decision link is
// a single-use HMAC that promotes whoever holds it to approver, so it is never a
// return value on any face (agent-interface §3.5).
func TestMCPLeaksNoSecretOrCapability(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "deploy-key", Kind: "age", Source: "user",
		Nonce: []byte("NONCE-SENTINEL"), Ciphertext: []byte("CIPHERTEXT-SENTINEL"), AAD: "aad",
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	job, approval := h.gatedJob(t, time.Hour)
	capability, _, err := reactive.ApprovalToken(h.env, job.ID)
	if err != nil {
		t.Fatalf("ApprovalToken: %v", err)
	}

	reader := h.mint(t, "ro", "readonly")
	approver := h.mint(t, "approver", "approve")

	var bodies []string
	for _, msg := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		rpcCall("list_secrets", `{}`),
		rpcCall("list_approvals", `{"all":true}`),
		rpcCall("get_job", `{"id":`+itoa64(job.ID)+`}`),
		rpcCall("get_job_events", `{"id":`+itoa64(job.ID)+`}`),
		`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"exocomp://approvals/pending"}}`,
	} {
		_, body := rpc(t, ts.URL, reader, msg)
		bodies = append(bodies, body)
	}
	_, decided := rpc(t, ts.URL, approver, rpcCall("approve_job", `{"approval_id":`+itoa64(approval.ID)+`}`))
	bodies = append(bodies, decided)

	for i, body := range bodies {
		for _, sentinel := range []string{
			"CIPHERTEXT-SENTINEL", "NONCE-SENTINEL", capability, "/approvals/" + capability,
		} {
			if strings.Contains(body, sentinel) {
				t.Errorf("response %d leaked %q: %s", i, sentinel, body)
			}
		}
	}
}
