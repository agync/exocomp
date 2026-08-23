package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/mcp"
	"exocomp.dev/exocomp/internal/store"
)

// newTestServer opens a real store under a temp dir and seeds a workspace and a
// secret, so the gate drives the same store reads and service enqueue the CLI
// and HTTP API drive.
func newTestServer(t *testing.T) (*mcp.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "web", TofuDir: "web"}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	// A secret row carrying a sentinel ciphertext: the metadata projection must
	// never surface it.
	if _, err := st.CreateSecret(ctx, store.SecretRow{
		Name: "deploy-key", Kind: "age", Source: "user",
		Nonce: []byte("NONCE-SENTINEL"), Ciphertext: []byte("CIPHERTEXT-SENTINEL"), AAD: "aad",
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	return mcp.New(st, dir, nil), st
}

// rpcResp is the decoded response envelope for assertions.
type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// drive runs a batch of newline-delimited request lines through the real stdio
// loop and returns the responses in order. Serve returns at EOF.
func drive(t *testing.T, srv *mcp.Server, lines ...string) []rpcResp {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var resps []rpcResp
	for _, ln := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if ln == "" {
			continue
		}
		var r rpcResp
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("decode response %q: %v", ln, err)
		}
		resps = append(resps, r)
	}
	return resps
}

// envelope is the wire success/error envelope a tool or resource emits.
type envelope struct {
	SchemaVersion int             `json:"schema_version"`
	Kind          string          `json:"kind"`
	Data          json.RawMessage `json:"data"`
	Error         *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// toolContent pulls the single text content block out of a tools/call result and
// decodes it as a wire envelope; it also returns isError.
func toolContent(t *testing.T, result json.RawMessage) (envelope, bool) {
	t.Helper()
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("want one text content block, got %+v", res.Content)
	}
	var env envelope
	if err := json.Unmarshal([]byte(res.Content[0].Text), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", res.Content[0].Text, err)
	}
	return env, res.IsError
}

func req(id int, method, params string) string {
	if params == "" {
		params = "{}"
	}
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `","params":` + params + `}`
}

func itoa(i int) string {
	return string(rune('0' + i)) // ids stay single-digit in these tests
}

// TestInitializeHandshake asserts initialize returns serverInfo + capabilities.
func TestInitializeHandshake(t *testing.T) {
	srv, _ := newTestServer(t)
	resps := drive(t, srv, req(1, "initialize", "{}"))
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("initialize: %+v", resps)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools     *struct{} `json:"tools"`
			Resources *struct{} `json:"resources"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocol version = %q", res.ProtocolVersion)
	}
	if res.Capabilities.Tools == nil || res.Capabilities.Resources == nil {
		t.Errorf("missing tool/resource capabilities: %s", resps[0].Result)
	}
	if res.ServerInfo.Name != "exocomp" || res.ServerInfo.Version == "" {
		t.Errorf("serverInfo = %+v", res.ServerInfo)
	}
}

// TestToolsListCatalog asserts the catalog advertises the read and write tools
// with input schemas, and that run_playbook is deferred.
func TestToolsListCatalog(t *testing.T) {
	srv, _ := newTestServer(t)
	resps := drive(t, srv, req(1, "tools/list", "{}"))
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	have := map[string]bool{}
	for _, tl := range res.Tools {
		if tl.Name == "" || len(tl.InputSchema) == 0 {
			t.Errorf("tool missing name/schema: %+v", tl)
		}
		have[tl.Name] = true
	}
	for _, want := range []string{
		"list_jobs", "get_job", "get_job_events", "list_workspaces", "get_workspace",
		"get_drift", "list_approvals", "list_sources", "list_schedules", "list_rules",
		"list_events", "explain_event", "list_secrets", "list_backups", "health",
		"workspace_plan", "workspace_apply",
	} {
		if !have[want] {
			t.Errorf("catalog missing tool %q", want)
		}
	}
	if have["run_playbook"] {
		t.Errorf("run_playbook should be deferred, not advertised")
	}
}

// TestReadToolWireShape drives list_workspaces and asserts the tool result is a
// wire success envelope carrying the seeded workspace.
func TestReadToolWireShape(t *testing.T) {
	srv, _ := newTestServer(t)
	resps := drive(t, srv, req(1, "tools/call", `{"name":"list_workspaces","arguments":{}}`))
	env, isErr := toolContent(t, resps[0].Result)
	if isErr {
		t.Fatalf("unexpected isError: %s", resps[0].Result)
	}
	if env.SchemaVersion != 1 || env.Kind != "workspace.list" {
		t.Errorf("envelope = version %d kind %q", env.SchemaVersion, env.Kind)
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "web" {
		t.Errorf("workspaces = %+v", rows)
	}
}

// TestWriteToolEnqueues drives workspace_plan and asserts a real queued job
// landed through the one queue (invariant 5): the row exists in the store with
// the workspace runbook and tofu kind, in the queued state.
func TestWriteToolEnqueues(t *testing.T) {
	srv, st := newTestServer(t)
	resps := drive(t, srv, req(1, "tools/call",
		`{"name":"workspace_plan","arguments":{"name":"web"}}`))
	env, isErr := toolContent(t, resps[0].Result)
	if isErr {
		t.Fatalf("workspace_plan errored: %s", resps[0].Result)
	}
	if env.Kind != "job.enqueued" {
		t.Fatalf("kind = %q", env.Kind)
	}
	var view struct {
		JobID int64  `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.JobID == 0 {
		t.Fatalf("no job id: %s", env.Data)
	}

	jobs, err := st.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want one enqueued job, got %d", len(jobs))
	}
	j := jobs[0]
	if j.ID != view.JobID {
		t.Errorf("job id: view %d store %d", view.JobID, j.ID)
	}
	if j.State != store.StateQueued {
		t.Errorf("state = %q, want queued (routed through the queue, not run)", j.State)
	}
	if j.Runbook != "workspace:web" || j.Kind != "tofu" {
		t.Errorf("job = runbook %q kind %q", j.Runbook, j.Kind)
	}
}

// TestResourcesListAndRead asserts resources/list advertises the collections and
// resources/read returns the same wire bytes its sibling tool would.
func TestResourcesListAndRead(t *testing.T) {
	srv, _ := newTestServer(t)
	resps := drive(t, srv,
		req(1, "resources/list", "{}"),
		req(2, "resources/read", `{"uri":"exocomp://workspaces"}`),
	)

	var list struct {
		Resources []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(resps[0].Result, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var haveWS bool
	for _, r := range list.Resources {
		if r.URI == "exocomp://workspaces" {
			haveWS = true
			if r.MimeType != "application/json" {
				t.Errorf("mimeType = %q", r.MimeType)
			}
		}
	}
	if !haveWS {
		t.Errorf("resources/list missing exocomp://workspaces: %s", resps[0].Result)
	}

	var read struct {
		Contents []struct {
			URI  string `json:"uri"`
			Text string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(resps[1].Result, &read); err != nil {
		t.Fatalf("decode read: %v", err)
	}
	if len(read.Contents) != 1 {
		t.Fatalf("want one content, got %d", len(read.Contents))
	}
	var env envelope
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &env); err != nil {
		t.Fatalf("decode resource envelope: %v", err)
	}
	if env.Kind != "workspace.list" {
		t.Errorf("resource kind = %q", env.Kind)
	}
}

// TestSecretToolMetadataOnly asserts list_secrets returns metadata and never the
// seeded ciphertext, nonce, or AAD.
func TestSecretToolMetadataOnly(t *testing.T) {
	srv, _ := newTestServer(t)
	resps := drive(t, srv, req(1, "tools/call", `{"name":"list_secrets","arguments":{}}`))
	env, isErr := toolContent(t, resps[0].Result)
	if isErr {
		t.Fatalf("list_secrets errored: %s", resps[0].Result)
	}
	if env.Kind != "secret.list" {
		t.Errorf("kind = %q", env.Kind)
	}
	body := string(env.Data)
	for _, forbidden := range []string{"CIPHERTEXT-SENTINEL", "NONCE-SENTINEL", "ciphertext", "nonce", "aad"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("secret metadata leaked %q: %s", forbidden, body)
		}
	}
	var rows []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "deploy-key" {
		t.Errorf("secrets = %+v", rows)
	}
}

// TestMalformedRequest asserts a line that is not valid JSON produces a JSON-RPC
// parse-error object, not a crash or silence.
func TestMalformedRequest(t *testing.T) {
	srv, _ := newTestServer(t)
	resps := drive(t, srv, `{"jsonrpc":"2.0","id":1,"method":`)
	if len(resps) != 1 {
		t.Fatalf("want one response, got %d", len(resps))
	}
	if resps[0].Error == nil || resps[0].Error.Code != -32700 {
		t.Fatalf("want parse error -32700, got %+v", resps[0].Error)
	}
	if string(resps[0].ID) != "null" {
		t.Errorf("parse-error id = %s, want null", resps[0].ID)
	}
}

// TestUnknownMethod asserts an unknown method is a method-not-found error.
func TestUnknownMethod(t *testing.T) {
	srv, _ := newTestServer(t)
	resps := drive(t, srv, req(1, "does/not/exist", "{}"))
	if resps[0].Error == nil || resps[0].Error.Code != -32601 {
		t.Fatalf("want method-not-found -32601, got %+v", resps[0].Error)
	}
}

// TestNotFoundToolError asserts a store not_found surfaces as an isError result
// carrying the taxonomy code, not a protocol error.
func TestNotFoundToolError(t *testing.T) {
	srv, _ := newTestServer(t)
	resps := drive(t, srv, req(1, "tools/call", `{"name":"get_job","arguments":{"id":999}}`))
	env, isErr := toolContent(t, resps[0].Result)
	if !isErr {
		t.Fatalf("want isError for missing job")
	}
	if env.Error == nil || env.Error.Code != "not_found" {
		t.Fatalf("want not_found, got %+v", env.Error)
	}
}

// TestListEventsRejectsUnknownOutcome asserts the handler enforces the outcome
// enum the schema advertises: a value outside the store's outcome set is a
// boundary error, not a silently-empty filter.
func TestListEventsRejectsUnknownOutcome(t *testing.T) {
	srv, _ := newTestServer(t)
	resps := drive(t, srv, req(1, "tools/call",
		`{"name":"list_events","arguments":{"outcome":"bogus"}}`))
	env, isErr := toolContent(t, resps[0].Result)
	if !isErr {
		t.Fatalf("want isError for unknown outcome")
	}
	if env.Error == nil || env.Error.Code != "invalid_argument" {
		t.Fatalf("want invalid_argument, got %+v", env.Error)
	}
}
