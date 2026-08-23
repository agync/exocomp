package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"exocomp.dev/exocomp/internal/mcp"
	"exocomp.dev/exocomp/internal/store"
)

// call dispatches one JSON-RPC message for a caller through the exported
// per-message seam the HTTP transport mounts on.
func call(t *testing.T, srv *mcp.Server, c mcp.Caller, line string) rpcResp {
	t.Helper()
	raw, write := srv.HandleMessage(context.Background(), c, []byte(line))
	if !write {
		t.Fatalf("no response for %s", line)
	}
	var r rpcResp
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return r
}

func toolNames(t *testing.T, result json.RawMessage) map[string]bool {
	t.Helper()
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	have := map[string]bool{}
	for _, tl := range list.Tools {
		have[tl.Name] = true
	}
	return have
}

// TestCatalogIsScopeFiltered asserts ADR 0014 §3's scope-gated tool visibility:
// a readonly token is advertised only read tools, an operator token also the
// write tools, and neither is advertised a decision tool.
func TestCatalogIsScopeFiltered(t *testing.T) {
	srv, _ := newTestServer(t)
	line := req(1, "tools/list", "{}")

	ro := toolNames(t, call(t, srv, mcp.TokenCaller(store.ScopeReadonly, "ro", 1, nil), line).Result)
	if !ro["list_jobs"] || !ro["get_drift"] {
		t.Errorf("readonly catalog is missing read tools: %v", ro)
	}
	for _, w := range []string{"workspace_plan", "workspace_apply"} {
		if ro[w] {
			t.Errorf("readonly token is advertised the write tool %q", w)
		}
	}

	op := toolNames(t, call(t, srv, mcp.TokenCaller(store.ScopeOperator, "op", 2, nil), line).Result)
	if !op["workspace_plan"] || !op["workspace_apply"] {
		t.Errorf("operator catalog is missing write tools: %v", op)
	}
	for _, a := range []string{"approve_job", "reject_job"} {
		if op[a] {
			t.Errorf("operator token is advertised the decision tool %q — W and A must stay disjoint", a)
		}
	}
}

// TestWriteToolRefusedBeforeStoreTouch is the per-call half of the scope gate: a
// readonly token calling a write tool is refused with the forbidden taxonomy
// code and NOTHING is enqueued, so the refusal lands ahead of the store, not
// after it.
func TestWriteToolRefusedBeforeStoreTouch(t *testing.T) {
	srv, st := newTestServer(t)
	resp := call(t, srv, mcp.TokenCaller(store.ScopeReadonly, "ro", 1, nil),
		req(1, "tools/call", `{"name":"workspace_plan","arguments":{"name":"web"}}`))

	env, isErr := toolContent(t, resp.Result)
	if !isErr {
		t.Fatalf("readonly write call was not refused: %s", resp.Result)
	}
	if env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("refusal code = %+v, want forbidden", env.Error)
	}
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused write enqueued %d job(s)", len(jobs))
	}
}

// TestUnknownScopeReadsNothing asserts the floor is explicit, not defaulted
// open: a caller whose scope this package does not recognize (a zero Caller, or
// a scope a future store adds) reads nothing at all.
func TestUnknownScopeReadsNothing(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, line := range []string{
		req(1, "tools/list", "{}"),
		req(2, "resources/read", `{"uri":"exocomp://workspaces"}`),
	} {
		resp := call(t, srv, mcp.Caller{}, line)
		if resp.Error == nil {
			t.Errorf("an unknown scope was served %s: %s", line, resp.Result)
		}
	}
}

// TestDecisionSeamNeedsBothScopeAndTransport asserts a decision capability is
// attached only under the approve scope: handing TokenCaller a decide seam with
// an operator scope drops it, so a write-capable token can never carry the
// release for the gate it raises (finding AI-2).
func TestDecisionSeamNeedsBothScopeAndTransport(t *testing.T) {
	decide := func(context.Context, int64, string) (mcp.Decision, error) {
		t.Fatal("the decision seam ran for a non-approve scope")
		return mcp.Decision{}, nil
	}
	srv, _ := newTestServer(t)
	line := req(1, "tools/list", "{}")

	op := toolNames(t, call(t, srv, mcp.TokenCaller(store.ScopeOperator, "op", 1, decide), line).Result)
	for _, a := range []string{"approve_job", "reject_job"} {
		if op[a] {
			t.Errorf("an operator token carrying a decide seam was advertised %q", a)
		}
	}
}
