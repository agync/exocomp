package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// bearerPost issues a POST with a bearer token and an optional JSON body, and
// returns status, headers, and body.
func bearerPost(t *testing.T, base, path, token, body string, hdr map[string]string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// decodeEnvelope pulls the kind and, if present, the error and data out of a
// wire envelope body.
func decodeEnvelope(t *testing.T, body string) (kind string, code string, data json.RawMessage) {
	t.Helper()
	var env struct {
		SchemaVersion int       `json:"schema_version"`
		Kind          string    `json:"kind"`
		Error         *struct { // present only on error envelopes
			Code string `json:"code"`
		} `json:"error"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("body is not a wire envelope: %v (%s)", err, body)
	}
	if env.Error != nil {
		return env.Kind, env.Error.Code, env.Data
	}
	return env.Kind, "", env.Data
}

// seedWorkspace registers a workspace with a real tofu dir so plan/apply resolve
// it by name.
func (h *harness) seedWorkspace(t *testing.T, name string) store.Workspace {
	t.Helper()
	ws, err := h.store.CreateWorkspace(context.Background(), store.Workspace{Name: name, TofuDir: t.TempDir()})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return ws
}

// TestAPIPlanEnqueuesThroughService: an operator token POSTing a plan gets a
// {job_id} wire envelope, and the row it created carries the runbook and
// concurrency key ONLY the service verb's spec assembly sets — proof the write
// went through internal/service and the queue, never a direct mutation.
func TestAPIPlanEnqueuesThroughService(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "web")

	code, hdr, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/plan", token, "", nil)
	if code != http.StatusOK {
		t.Fatalf("plan status = %d, want 200 (%s)", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("plan content-type = %q, want application/json", ct)
	}
	kind, _, data := decodeEnvelope(t, body)
	if kind != kindJobEnqueued {
		t.Fatalf("plan kind = %q, want %q (%s)", kind, kindJobEnqueued, body)
	}
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil || jv.JobID == 0 {
		t.Fatalf("plan data has no job_id: %v (%s)", err, body)
	}

	job, err := h.store.GetJob(context.Background(), jv.JobID)
	if err != nil {
		t.Fatalf("GetJob(%d): %v", jv.JobID, err)
	}
	if job.Runbook != service.WorkspaceRunbook("web") {
		t.Fatalf("job runbook = %q, want %q — write did not route through the service verb",
			job.Runbook, service.WorkspaceRunbook("web"))
	}
	if job.ConcurrencyKey != "tofu:web" {
		t.Fatalf("job concurrency_key = %q, want tofu:web", job.ConcurrencyKey)
	}
}

// TestAPIPlanCarriesRunnerName: a plan request naming a runner sets
// JobArgs.RunnerName on the enqueued row, so the daemon's existing caps
// resolution + fail-closed refusal applies to a remote-submitted job exactly as
// it does for a local one.
func TestAPIPlanCarriesRunnerName(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "web")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/plan", token,
		`{"runner":"fedora-ee"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("plan status = %d, want 200 (%s)", code, body)
	}
	_, _, data := decodeEnvelope(t, body)
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil || jv.JobID == 0 {
		t.Fatalf("plan data has no job_id: %v (%s)", err, body)
	}
	job, err := h.store.GetJob(context.Background(), jv.JobID)
	if err != nil {
		t.Fatalf("GetJob(%d): %v", jv.JobID, err)
	}
	if job.Args.RunnerName != "fedora-ee" {
		t.Fatalf("job.Args.RunnerName = %q, want fedora-ee", job.Args.RunnerName)
	}
}

// TestAPIApplyCarriesRunnerName: same as the plan case, for a fresh apply.
func TestAPIApplyCarriesRunnerName(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "web")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/apply", token,
		`{"runner":"fedora-ee"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, want 200 (%s)", code, body)
	}
	_, _, data := decodeEnvelope(t, body)
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil || jv.JobID == 0 {
		t.Fatalf("apply data has no job_id: %v (%s)", err, body)
	}
	job, err := h.store.GetJob(context.Background(), jv.JobID)
	if err != nil {
		t.Fatalf("GetJob(%d): %v", jv.JobID, err)
	}
	if job.Args.RunnerName != "fedora-ee" {
		t.Fatalf("job.Args.RunnerName = %q, want fedora-ee", job.Args.RunnerName)
	}
}

// TestAPIPlanScopeGate: a readonly token is forbidden (403 wire error) and no
// token is unauthenticated (401), both before any job is created.
func TestAPIPlanScopeGate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")

	ro := h.mint(t, "ro", "readonly")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/plan", ro, "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("readonly plan status = %d, want 403 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "forbidden" {
		t.Fatalf("readonly plan envelope = %s, want error/forbidden", body)
	}

	code, hdr, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/plan", "", "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token plan status = %d, want 401 (%s)", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("no-token plan content-type = %q, want application/json", ct)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "unauthenticated" {
		t.Fatalf("no-token plan envelope = %s, want error/unauthenticated", body)
	}

	// The scope gate ran before any work: the store holds no job for the workspace.
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: service.WorkspaceRunbook("web")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused write still created %d job(s)", len(jobs))
	}
}

// TestAPIWriteMethodMismatch: a GET on a write path, and a non-POST method, both
// fall through to the mux's 405 — Go 1.22 method-aware routing, the contract's
// write/read method-mismatch rule.
func TestAPIWriteMethodMismatch(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "web")

	code, _, _ := bearerGet(t, ts.URL, "/api/v1/workspaces/web/plan", token)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a write path status = %d, want 405", code)
	}
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(m, ts.URL+"/api/v1/workspaces/web/plan", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s plan: %v", m, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s on a write path status = %d, want 405", m, resp.StatusCode)
		}
	}
}

// TestAPIPlanIdempotencyKey: two plans with the same Idempotency-Key dedup to
// one job, so a retrying agent never double-enqueues.
func TestAPIPlanIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "web")

	hdr := map[string]string{"Idempotency-Key": "retry-42"}
	firstID := postPlanJobID(t, ts.URL, token, hdr)
	secondID := postPlanJobID(t, ts.URL, token, hdr)
	if firstID != secondID {
		t.Fatalf("idempotency-key produced two jobs: %d and %d", firstID, secondID)
	}
}

func postPlanJobID(t *testing.T, base, token string, hdr map[string]string) int64 {
	t.Helper()
	code, _, body := bearerPost(t, base, "/api/v1/workspaces/web/plan", token, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("plan status = %d, want 200 (%s)", code, body)
	}
	_, _, data := decodeEnvelope(t, body)
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil {
		t.Fatalf("plan data: %v", err)
	}
	return jv.JobID
}

// TestAPIApplyFreshEnqueues: a fresh apply (no saved plan) enqueues an
// auto-approve run and returns its job id.
func TestAPIApplyFreshEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "web")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/apply", token, "", nil)
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, want 200 (%s)", code, body)
	}
	kind, _, data := decodeEnvelope(t, body)
	if kind != kindJobEnqueued {
		t.Fatalf("apply kind = %q, want %q (%s)", kind, kindJobEnqueued, body)
	}
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil || jv.JobID == 0 {
		t.Fatalf("apply data has no job_id: %v (%s)", err, body)
	}
	job, err := h.store.GetJob(context.Background(), jv.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Runbook != service.WorkspaceRunbook("web") {
		t.Fatalf("apply job runbook = %q, want %q", job.Runbook, service.WorkspaceRunbook("web"))
	}
}

// TestAPIApplyGatedReturnsPending: an apply of a saved plan whose recorded
// verdict is require_approval lands the job in awaiting_approval behind a fresh
// approval — the gate path (EnqueueGated), NOT a bare enqueue. The response is
// the pending envelope with a ttl.
func TestAPIApplyGatedReturnsPending(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	ctx := context.Background()
	h.seedWorkspace(t, "web")

	// A prior plan job, its sealed plan artifact, and the values-free policy
	// verdict recorded on that job's timeline — the shape `workspace plan` leaves.
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
	if code != http.StatusOK {
		t.Fatalf("gated apply status = %d, want 200 (%s)", code, body)
	}
	kind, _, data := decodeEnvelope(t, body)
	if kind != kindApprovalPending {
		t.Fatalf("gated apply kind = %q, want %q (%s)", kind, kindApprovalPending, body)
	}
	var av approvalPendingView
	if err := json.Unmarshal(data, &av); err != nil {
		t.Fatalf("pending data: %v", err)
	}
	if av.ApprovalID == 0 || av.JobID == 0 {
		t.Fatalf("pending envelope missing ids: %s", body)
	}
	if av.State != string(store.ApprovalPending) {
		t.Fatalf("pending state = %q, want %q", av.State, store.ApprovalPending)
	}
	if av.TTLSeconds <= 0 {
		t.Fatalf("pending ttl_seconds = %d, want > 0", av.TTLSeconds)
	}

	// The job went through EnqueueGated: it sits in awaiting_approval, a state no
	// worker can claim — a bare Enqueue would have left it queued.
	gated, err := h.store.GetJob(ctx, av.JobID)
	if err != nil {
		t.Fatalf("GetJob(gated): %v", err)
	}
	if gated.State != store.StateAwaitingApproval {
		t.Fatalf("gated job state = %q, want awaiting_approval (not a bare enqueue)", gated.State)
	}
	if _, err := h.store.GetApproval(ctx, av.ApprovalID); err != nil {
		t.Fatalf("gated apply created no approval row: %v", err)
	}
}

// TestAPIApplyDeniedIsConflict: an apply of a saved plan whose verdict is deny is
// refused with 409 before anything is enqueued.
func TestAPIApplyDeniedIsConflict(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	ctx := context.Background()
	h.seedWorkspace(t, "web")

	planJob, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook("web"), Kind: "tofu", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	art, err := h.store.CreateArtifact(ctx, store.Artifact{
		JobID: planJob.ID, Kind: reactive.PlanArtifactKind, Path: "/dev/null", SHA256: "x", Size: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	raw := fmt.Sprintf(`{"plan_policy":{"action":"deny","policy":"no-prod","artifact":%d}}`, art.ID)
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: planJob.ID, Level: store.LevelInfo, Msg: "policy", RawJSON: raw,
	}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/apply", token,
		fmt.Sprintf(`{"plan":"%d"}`, art.ID), nil)
	if code != http.StatusConflict {
		t.Fatalf("denied apply status = %d, want 409 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "conflict" {
		t.Fatalf("denied apply envelope = %s, want error/conflict", body)
	}
}

// TestAPIDecisionApproveReleases: an approve decision by an approve-scope token
// settles the approval through the reactive engine and releases the gated job,
// which the running workers then run to success. A readonly token is forbidden;
// so is an operator token (see api_decision_scope_test.go): a token that may
// raise the gate may not also release it.
func TestAPIDecisionApproveReleases(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// readonly may not decide.
	job, approval := h.gatedJob(t, time.Hour)
	ro := h.mint(t, "ro", "readonly")
	code, _, body := bearerPost(t, ts.URL,
		"/api/v1/approvals/"+itoa64(approval.ID)+"/decision", ro, `{"decision":"approve"}`, nil)
	if code != http.StatusForbidden {
		t.Fatalf("readonly decision status = %d, want 403 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "forbidden" {
		t.Fatalf("readonly decision envelope = %s, want error/forbidden", body)
	}

	// the approve scope approves: 200, decision envelope, job releases and runs.
	op := h.mint(t, "approver", "approve")
	code, _, body = bearerPost(t, ts.URL,
		"/api/v1/approvals/"+itoa64(approval.ID)+"/decision", op, `{"decision":"approve"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200 (%s)", code, body)
	}
	kind, _, data := decodeEnvelope(t, body)
	if kind != kindDecision {
		t.Fatalf("approve kind = %q, want %q (%s)", kind, kindDecision, body)
	}
	var dv decisionView
	if err := json.Unmarshal(data, &dv); err != nil || dv.ApprovalID != approval.ID {
		t.Fatalf("decision data: %v (%s)", err, body)
	}
	h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)
}

// TestAPIDecisionBadBody: a decision that is neither approve nor reject is a 400.
func TestAPIDecisionBadBody(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	_, approval := h.gatedJob(t, time.Hour)
	op := h.mint(t, "approver", "approve")

	code, _, body := bearerPost(t, ts.URL,
		"/api/v1/approvals/"+itoa64(approval.ID)+"/decision", op, `{"decision":"maybe"}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("bad decision status = %d, want 400 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != codeInvalidArgument {
		t.Fatalf("bad decision envelope = %s, want error/invalid_argument", body)
	}
}

// TestAPIWriteUnknownWorkspaceIs404: a plan on a workspace that does not exist is
// a 404 with the not_found code, through the same apiFail taxonomy the reads use.
func TestAPIWriteUnknownWorkspaceIs404(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/ghost/plan", token, "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown workspace status = %d, want 404 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "not_found" {
		t.Fatalf("unknown workspace envelope = %s, want error/not_found", body)
	}
}

// ensure the reactive decision constant is the one the handler validates against
// (guards a rename from silently loosening the check).
var _ = reactive.DecisionReject
