package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/wire"
)

// writeEnvelope answers a write request the way internal/web/api_write.go does.
func writeEnvelope(t *testing.T, w http.ResponseWriter, kind string, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := wire.WriteSuccess(w, kind, data); err != nil {
		t.Errorf("write envelope: %v", err)
	}
}

func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("body is not JSON (%q): %v", raw, err)
		}
	}
	return out
}

// TestWorkspaceRequestBodyCarriesRunner: body() carries "runner" when set and
// omits it cleanly when empty, mirroring run_secrets/timeout/plan above.
func TestWorkspaceRequestBodyCarriesRunner(t *testing.T) {
	with := WorkspaceRequest{Runner: "fedora-ee"}.body()
	if with["runner"] != "fedora-ee" {
		t.Fatalf("body = %+v, want runner = fedora-ee", with)
	}

	without := WorkspaceRequest{}.body()
	if _, ok := without["runner"]; ok {
		t.Fatalf("body = %+v, want no runner key when unset", without)
	}
}

func TestPlanWorkspacePostsAndDecodes(t *testing.T) {
	var gotPath, gotMethod, gotIdem string
	var gotBody map[string]any
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotIdem = r.URL.Path, r.Method, r.Header.Get("Idempotency-Key")
		gotBody = readBody(t, r)
		writeEnvelope(t, w, KindJobEnqueued, JobEnqueued{JobID: 42, State: "queued"})
	}))

	c := newTestClient(t, srv, fp)
	got, err := c.PlanWorkspace(context.Background(), "edge", WorkspaceRequest{
		RunSecrets: "aws", Timeout: 90 * time.Second, IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatalf("PlanWorkspace: %v", err)
	}
	if got.JobID != 42 || got.State != "queued" {
		t.Fatalf("decoded %+v", got)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/workspaces/edge/plan" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	if gotIdem != "key-1" {
		t.Errorf("Idempotency-Key = %q", gotIdem)
	}
	if gotBody["run_secrets"] != "aws" || gotBody["timeout"] != "1m30s" {
		t.Errorf("body = %+v", gotBody)
	}
}

// An apply answers with EITHER kind; the gated one must not read as a started run.
func TestApplyWorkspaceGatedReturnsPending(t *testing.T) {
	want := ApprovalPending{
		ApprovalID: 7, JobID: 12, State: "awaiting_approval",
		ExpiresAt: "2026-08-23T10:30:00Z", TTLSeconds: 1800,
		Policy: "prod-guard", PlanArtifactID: 3,
	}
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body := readBody(t, r); body["plan"] != "3" {
			t.Errorf("plan not forwarded: %+v", body)
		}
		writeEnvelope(t, w, KindApprovalPending, want)
	}))

	c := newTestClient(t, srv, fp)
	got, err := c.ApplyWorkspace(context.Background(), "edge", WorkspaceRequest{Plan: "3"})
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	if got.Enqueued != nil {
		t.Fatalf("a gated apply must not report an enqueued job: %+v", got.Enqueued)
	}
	if got.Pending == nil || *got.Pending != want {
		t.Fatalf("pending = %+v, want %+v", got.Pending, want)
	}
}

func TestApplyWorkspaceEnqueued(t *testing.T) {
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(t, w, KindJobEnqueued, JobEnqueued{JobID: 5, State: "queued"})
	}))
	c := newTestClient(t, srv, fp)
	got, err := c.ApplyWorkspace(context.Background(), "edge", WorkspaceRequest{})
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	if got.Pending != nil || got.Enqueued == nil || got.Enqueued.JobID != 5 {
		t.Fatalf("result = %+v", got)
	}
}

func TestRunPlaybookPostsRequest(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = readBody(t, r)
		writeEnvelope(t, w, KindJobEnqueued, JobEnqueued{JobID: 9, State: "queued"})
	}))

	c := newTestClient(t, srv, fp)
	got, err := c.RunPlaybook(context.Background(), PlaybookRequest{
		Playbook: "site.yml", Source: "demo", Inventory: "lab",
		Check: true, ExtraVars: []string{"marker=one"}, IdempotencyKey: "k",
		Runner: "fedora-ee",
	})
	if err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	if got.JobID != 9 {
		t.Fatalf("job = %+v", got)
	}
	if gotPath != "/api/v1/playbooks/run" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["playbook"] != "site.yml" || gotBody["source"] != "demo" ||
		gotBody["inventory"] != "lab" || gotBody["check"] != true || gotBody["runner"] != "fedora-ee" {
		t.Errorf("body = %+v", gotBody)
	}
	vars, _ := gotBody["extra_vars"].([]any)
	if len(vars) != 1 || vars[0] != "marker=one" {
		t.Errorf("extra_vars = %+v", gotBody["extra_vars"])
	}
	// A key=val extra var travels in the body, never as an argv fragment the
	// daemon would have to re-split.
	if _, ok := gotBody["argv"]; ok {
		t.Error("request must not carry argv")
	}
}

func TestDecideApprovalPostsDecision(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = readBody(t, r)
		writeEnvelope(t, w, KindDecision, Decision{
			ApprovalID: 7, Outcome: "approved", Message: "approval 7 approved; job 12 released",
		})
	}))

	c := newTestClient(t, srv, fp)
	got, err := c.DecideApproval(context.Background(), 7, "approve")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if gotPath != "/api/v1/approvals/7/decision" || gotBody["decision"] != "approve" {
		t.Fatalf("posted %s %+v", gotPath, gotBody)
	}
	if got.Outcome != "approved" || got.ApprovalID != 7 {
		t.Fatalf("decision = %+v", got)
	}
	// The capability token is derived server-side: nothing bearer-like is sent.
	if _, ok := gotBody["token"]; ok {
		t.Error("a decision must not carry a token")
	}
}

// The wire error path: a refusal keeps its taxonomy code and its exit code.
func TestWriteErrorEnvelopeIsTypedAPIError(t *testing.T) {
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = wire.WriteErrorValue(w, wire.NewError(wire.CodeForbidden, "this token's scope may not enqueue writes"))
	}))
	c := newTestClient(t, srv, fp)
	_, err := c.PlanWorkspace(context.Background(), "edge", WorkspaceRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != wire.CodeForbidden || apiErr.Status != http.StatusForbidden {
		t.Fatalf("error = %+v", apiErr)
	}
	if apiErr.ExitCode() != wire.ExitForCode(wire.CodeForbidden) {
		t.Errorf("exit = %d", apiErr.ExitCode())
	}
}

// A conflict (a denied plan) is a refusal, not a transport failure.
func TestApplyDeniedPlanIsConflict(t *testing.T) {
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = wire.WriteErrorValue(w, wire.NewError(wire.CodeConflict, `plan 3 is unconsumable: denied by policy "prod-guard"`))
	}))
	c := newTestClient(t, srv, fp)
	_, err := c.ApplyWorkspace(context.Background(), "edge", WorkspaceRequest{Plan: "3"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != wire.CodeConflict {
		t.Fatalf("want a conflict APIError, got %v", err)
	}
}

// A 200 carrying an unexpected kind is refused rather than silently decoded into
// a zero job id a caller would then try to follow.
func TestWriteRejectsUnexpectedKind(t *testing.T) {
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(t, w, wire.KindJobList, []wire.JobView{})
	}))
	c := newTestClient(t, srv, fp)
	if _, err := c.PlanWorkspace(context.Background(), "edge", WorkspaceRequest{}); err == nil {
		t.Fatal("want a refusal of the wrong kind")
	}
}

// The decoded write view re-emitted through wire.WriteSuccess must be
// byte-identical to what the daemon wrote: that is what makes a remote
// `--output json` write equal the local one.
func TestWriteBytesMatchServer(t *testing.T) {
	pending := ApprovalPending{
		ApprovalID: 7, JobID: 12, State: "awaiting_approval",
		ExpiresAt: "2026-08-23T10:30:00Z", TTLSeconds: 1800, Policy: "prod-guard", PlanArtifactID: 3,
	}
	serverBytes, err := wire.MarshalSuccess(KindApprovalPending, pending)
	if err != nil {
		t.Fatal(err)
	}
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(serverBytes)
	}))
	c := newTestClient(t, srv, fp)
	got, err := c.ApplyWorkspace(context.Background(), "edge", WorkspaceRequest{Plan: "3"})
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	reEmitted, err := wire.MarshalSuccess(KindApprovalPending, got.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if string(reEmitted) != string(serverBytes) {
		t.Fatalf("re-emitted bytes differ:\n server: %s\n client: %s", serverBytes, reEmitted)
	}
}

// A workspace name with a path separator addresses the row it names; it must not
// be spliced raw into the URL where it would select a different route.
func TestWorkspaceNameIsPathEscaped(t *testing.T) {
	var gotPath string
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		writeEnvelope(t, w, KindJobEnqueued, JobEnqueued{JobID: 1})
	}))
	c := newTestClient(t, srv, fp)
	if _, err := c.PlanWorkspace(context.Background(), "edge/prod", WorkspaceRequest{}); err != nil {
		t.Fatalf("PlanWorkspace: %v", err)
	}
	if gotPath != "/api/v1/workspaces/edge%2Fprod/plan" {
		t.Fatalf("escaped path = %q", gotPath)
	}
}
