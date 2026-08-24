package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// jsonrpcResp is one decoded JSON-RPC response frame.
type jsonrpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpc POSTs one JSON-RPC message to the mounted MCP endpoint and returns the
// status and the raw body, so a test can assert on a transport-level refusal as
// easily as on a protocol frame.
func rpc(t *testing.T, base, token, body string) (int, string) {
	t.Helper()
	code, _, out := bearerPost(t, base, mcpPath, token, body, nil)
	return code, out
}

// rpcOK POSTs one message, requires HTTP 200, and decodes the response frame.
func rpcOK(t *testing.T, base, token, body string) jsonrpcResp {
	t.Helper()
	code, out := rpc(t, base, token, body)
	if code != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200 (%s)", mcpPath, code, out)
	}
	var resp jsonrpcResp
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("body is not a JSON-RPC frame: %v (%s)", err, out)
	}
	return resp
}

// mcpToolNames decodes a tools/list result into a name set.
func mcpToolNames(t *testing.T, result json.RawMessage) map[string]bool {
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

// mcpToolEnvelope pulls the wire envelope out of a tools/call result.
func mcpToolEnvelope(t *testing.T, result json.RawMessage) (kind, code string, data json.RawMessage, isError bool) {
	t.Helper()
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		t.Fatalf("decode tool result: %v (%s)", err, result)
	}
	if len(res.Content) != 1 {
		t.Fatalf("want one content block, got %d", len(res.Content))
	}
	kind, code, data = decodeEnvelope(t, res.Content[0].Text)
	return kind, code, data, res.IsError
}

func rpcCall(name, args string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
}

// TestMCPRequiresBearer is the auth floor: an unauthenticated MCP request is
// refused at the TRANSPORT, with the same 401 and wire envelope /api/v1 renders
// — never a 200 carrying a JSON-RPC frame, which would tell an agent it had a
// session it does not have.
func TestMCPRequiresBearer(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	code, body := rpc(t, ts.URL, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "unauthenticated" {
		t.Fatalf("unauthenticated envelope = %s, want error/unauthenticated", body)
	}
	if strings.Contains(body, "jsonrpc") {
		t.Fatalf("a refused request was answered with a JSON-RPC frame: %s", body)
	}
}

// TestMCPRejectsCookieAuth keeps the machine face free of ambient credentials:
// a browser session must not drive MCP tools, however the browser was talked
// into sending it.
func TestMCPRejectsCookieAuth(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	token := h.mint(t, "op", "operator")
	client := login(t, ts.URL, token)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+mcpPath,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("cookie POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie-authenticated MCP status = %d, want 403", resp.StatusCode)
	}
}

// TestMCPHandshakeAndResourceRead drives the initialize handshake and a
// resources/read over HTTP, and asserts the resource body is the same wire
// envelope every other face marshals.
func TestMCPHandshakeAndResourceRead(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	token := h.mint(t, "ro", "readonly")

	init := rpcOK(t, ts.URL, token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if init.Error != nil {
		t.Fatalf("initialize: %+v", init.Error)
	}
	var hs struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(init.Result, &hs); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if hs.ProtocolVersion != "2024-11-05" || hs.ServerInfo.Name != "exocomp" {
		t.Fatalf("handshake = %+v", hs)
	}

	read := rpcOK(t, ts.URL, token,
		`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"exocomp://workspaces"}}`)
	if read.Error != nil {
		t.Fatalf("resources/read: %+v", read.Error)
	}
	var body struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(read.Result, &body); err != nil {
		t.Fatalf("decode resources/read: %v", err)
	}
	if len(body.Contents) != 1 || body.Contents[0].MimeType != "application/json" {
		t.Fatalf("resource contents = %+v", body.Contents)
	}
	if kind, _, _ := decodeEnvelope(t, body.Contents[0].Text); kind != "workspace.list" {
		t.Fatalf("resource kind = %q, want workspace.list", kind)
	}
}

// TestMCPReadonlyTokenSeesNoWriteTools is ADR 0014 §3's scope-gated visibility
// on the HTTP face, plus its per-call half: a readonly token is advertised no
// write tool, and calling one anyway is refused BEFORE the store is touched.
func TestMCPReadonlyTokenSeesNoWriteTools(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	h.seedWorkspace(t, "web")
	token := h.mint(t, "ro", "readonly")

	list := rpcOK(t, ts.URL, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	have := mcpToolNames(t, list.Result)
	if !have["list_jobs"] {
		t.Fatalf("readonly catalog has no read tools: %v", have)
	}
	for _, w := range []string{"workspace_plan", "workspace_apply", "approve_job", "reject_job"} {
		if have[w] {
			t.Errorf("readonly token is advertised %q", w)
		}
	}

	call := rpcOK(t, ts.URL, token, rpcCall("workspace_plan", `{"name":"web"}`))
	kind, code, _, isErr := mcpToolEnvelope(t, call.Result)
	if !isErr || kind != "error" || code != "forbidden" {
		t.Fatalf("readonly write call = kind %q code %q isError %v, want error/forbidden", kind, code, isErr)
	}
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused MCP write enqueued %d job(s)", len(jobs))
	}
}

// TestMCPWriteToolEnqueuesThroughService proves invariant 5 across the new
// transport: an operator token's tool call lands a real queued row, carrying the
// runbook and kind only the service verb's spec assembly sets, attributed to the
// token behind it rather than to the local operator.
func TestMCPWriteToolEnqueuesThroughService(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	h.seedWorkspace(t, "web")
	token := h.mint(t, "agent", "operator")

	call := rpcOK(t, ts.URL, token, rpcCall("workspace_plan", `{"name":"web"}`))
	kind, code, data, isErr := mcpToolEnvelope(t, call.Result)
	if isErr {
		t.Fatalf("workspace_plan failed: kind %q code %q", kind, code)
	}
	if kind != "job.enqueued" {
		t.Fatalf("kind = %q, want job.enqueued", kind)
	}
	var view struct {
		JobID int64  `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &view); err != nil || view.JobID == 0 {
		t.Fatalf("no job id: %v (%s)", err, data)
	}

	job, err := h.store.GetJob(ctx, view.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Runbook != service.WorkspaceRunbook("web") || job.Kind != "tofu" {
		t.Fatalf("job = runbook %q kind %q", job.Runbook, job.Kind)
	}
	events, err := h.store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var attributed bool
	for _, e := range events {
		if strings.Contains(e.Msg, "via mcp") && strings.Contains(e.Msg, "agent") {
			attributed = true
		}
	}
	if !attributed {
		t.Fatalf("job %d is not attributed to the MCP token: %+v", job.ID, events)
	}
}

// TestMCPRevokedTokenFailsOnNextCall mirrors the SSE tail's revoke behaviour on
// this face: the transport is stateless and re-resolves the credential per
// message, so a revoke bites on the very next call instead of at the end of a
// long-lived session.
func TestMCPRevokedTokenFailsOnNextCall(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "leaked", "operator")

	if resp := rpcOK(t, ts.URL, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`); resp.Error != nil {
		t.Fatalf("first call: %+v", resp.Error)
	}
	if err := h.store.RevokeToken(context.Background(), "leaked"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	code, body := rpc(t, ts.URL, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("post-revoke status = %d, want 401 (%s)", code, body)
	}
}

// TestMCPMalformedMessages asserts the protocol boundary: an unparseable body is
// a JSON-RPC parse error, an unknown method is method-not-found, and neither is
// a crash, a hang, or an empty 200.
func TestMCPMalformedMessages(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "ro", "readonly")

	garbled := rpcOK(t, ts.URL, token, `{"jsonrpc":"2.0","id":1,"method":`)
	if garbled.Error == nil || garbled.Error.Code != -32700 {
		t.Fatalf("malformed body error = %+v, want -32700", garbled.Error)
	}
	if string(garbled.ID) != "null" {
		t.Errorf("parse-error id = %s, want null", garbled.ID)
	}

	unknown := rpcOK(t, ts.URL, token, `{"jsonrpc":"2.0","id":2,"method":"does/not/exist","params":{}}`)
	if unknown.Error == nil || unknown.Error.Code != -32601 {
		t.Fatalf("unknown method error = %+v, want -32601", unknown.Error)
	}

	// A notification has no id to answer on, so the transport acknowledges it
	// with 202 and no body rather than inventing a frame.
	code, body := rpc(t, ts.URL, token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted || strings.TrimSpace(body) != "" {
		t.Fatalf("notification response = %d %q, want 202 and an empty body", code, body)
	}
}

// TestMCPGetIsNotAStream documents what this transport does NOT implement: there
// is no server-initiated stream to open, so the endpoint is POST-only and a GET
// is a method error rather than a hanging SSE connection.
func TestMCPGetIsNotAStream(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "ro", "readonly")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+mcpPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", mcpPath, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
}
