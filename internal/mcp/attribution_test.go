package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestWriteToolIsAttributed proves an MCP-enqueued write carries an Actor.
// Without one the job ledger would read exactly like a local CLI run by the
// operator — an agent's action attributed to the human. Every MCP write now
// names the face that asked.
func TestWriteToolIsAttributed(t *testing.T) {
	srv, st := newTestServer(t)
	resps := drive(t, srv, req(1, "tools/call", `{"name":"workspace_plan","arguments":{"name":"web"}}`))
	env, isErr := toolContent(t, resps[0].Result)
	if isErr {
		t.Fatalf("workspace_plan failed: %s", resps[0].Result)
	}
	var view struct {
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal(env.Data, &view); err != nil || view.JobID == 0 {
		t.Fatalf("no job id: %v (%s)", err, env.Data)
	}

	events, err := st.ListJobEvents(context.Background(), view.JobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if strings.Contains(e.Msg, "via mcp") {
			found = true
		}
	}
	if !found {
		t.Fatalf("job %d carries no mcp attribution: %+v", view.JobID, events)
	}
}

// TestNotificationCannotMutate proves a tools/call sent as
// a JSON-RPC NOTIFICATION enqueued the job and wrote zero bytes back, so a
// mutation happened with nothing for the harness — or an audit wrapper on the
// frames — to record. A side-effecting method must carry an id.
func TestNotificationCannotMutate(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	before, err := st.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}

	resps := drive(t, srv,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"workspace_plan","arguments":{"name":"web"}}}`)

	after, err := st.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("a notification enqueued %d job(s)", len(after)-len(before))
	}
	if len(resps) != 1 || resps[0].Error == nil {
		t.Fatalf("a side-effecting notification must be refused with an error, got %+v", resps)
	}
	if resps[0].Error.Code != -32600 {
		t.Errorf("error code = %d, want -32600 (invalid request)", resps[0].Error.Code)
	}

	// A plain notification the protocol expects (initialized) is still ignored
	// silently: only side-effecting methods are refused.
	if extra := drive(t, srv, `{"jsonrpc":"2.0","method":"ping"}`); len(extra) != 0 {
		t.Errorf("a harmless notification drew a response: %+v", extra)
	}
}
