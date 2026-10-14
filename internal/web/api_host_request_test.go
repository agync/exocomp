package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

func TestAPIHostRequestEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	if _, err := h.store.CreateRule(context.Background(), store.NewRule{
		Name:         "host-ping",
		SourceFilter: reactive.SourceHost,
		CELExpr:      `event.name == "ping"`,
		ActionKind:   store.ActionShell,
		Action:       store.RuleAction{Argv: []string{"/bin/sh", "-c", "echo ok; exit 0"}},
	}); err != nil {
		t.Fatal(err)
	}

	token, _, err := MintToken(context.Background(), h.store, "host-node", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}

	code, _, body := bearerPost(t, ts.URL, "/api/v1/hosts/self/requests", token,
		`{"request":"ping"}`, nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", code, body)
	}
	var env struct {
		Data struct {
			Outcome string `json:"outcome"`
			JobID   *int64 `json:"job_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Outcome != "fired" || env.Data.JobID == nil {
		t.Fatalf("outcome = %q job_id = %v, want fired with job", env.Data.Outcome, env.Data.JobID)
	}
}

func TestAPIHostRequestRejectsUnknownRequest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	token, _, err := MintToken(context.Background(), h.store, "host-node2", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}
	code, _, body := bearerPost(t, ts.URL, "/api/v1/hosts/self/requests", token,
		`{"request":"bogus"}`, nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", code, body)
	}
	if !strings.Contains(body, `"ignored"`) {
		t.Fatalf("expected ignored outcome, got %s", body)
	}
}

func TestAPIHostRequestScopeIsolated(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// A host token must not be able to read the job list.
	token, _, err := MintToken(context.Background(), h.store, "host-node3", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /jobs with host token status = %d, want 403", resp.StatusCode)
	}
}

// hostRequestResp is the data block of a host.request response.
type hostRequestResp struct {
	Outcome    string `json:"outcome"`
	JobID      *int64 `json:"job_id"`
	ApprovalID *int64 `json:"approval_id"`
	JobState   string `json:"job_state"`
	ExitCode   *int   `json:"exit_code"`
}

// pingRule registers a rule that runs argv when a host request names it, the
// shape the host self-service path converges on.
func pingRule(t *testing.T, h *harness, name, argv string, requireApproval bool) {
	t.Helper()
	if _, err := h.store.CreateRule(context.Background(), store.NewRule{
		Name:            name,
		SourceFilter:    reactive.SourceHost,
		CELExpr:         `event.name == "` + name + `"`,
		ActionKind:      store.ActionShell,
		Action:          store.RuleAction{Argv: []string{"/bin/sh", "-c", argv}},
		RequireApproval: requireApproval,
	}); err != nil {
		t.Fatal(err)
	}
}

// postHostRequest posts one host request and decodes the response data.
func postHostRequest(t *testing.T, base, token, request, query string, hdr map[string]string) (int, hostRequestResp, string) {
	t.Helper()
	code, _, body := bearerPost(t, base, "/api/v1/hosts/self/requests"+query, token,
		`{"request":"`+request+`"}`, hdr)
	var env struct {
		Data hostRequestResp `json:"data"`
	}
	if code == http.StatusAccepted {
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("response is not a host.request envelope: %v (%s)", err, body)
		}
	}
	return code, env.Data, body
}

// TestAPIHostRequestWaitReturnsTerminalState: the blocking path returns the
// job's terminal state and exit code in the same response.
func TestAPIHostRequestWaitReturnsTerminalState(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	pingRule(t, h, "ping", "echo ok; exit 0", false)
	token, _, err := MintToken(context.Background(), h.store, "host-wait", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}

	code, data, body := postHostRequest(t, ts.URL, token, "ping", "?wait=true&timeout=10s", nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", code, body)
	}
	if data.JobID == nil {
		t.Fatalf("job_id is nil (%s)", body)
	}
	if data.JobState != string(store.StateSucceeded) {
		t.Fatalf("job_state = %q, want %q (%s)", data.JobState, store.StateSucceeded, body)
	}
	if data.ExitCode == nil || *data.ExitCode != 0 {
		t.Fatalf("exit_code = %v, want 0 (%s)", data.ExitCode, body)
	}
}

// TestAPIHostRequestWaitDeadlineReturnsPending: a job that cannot terminalise —
// an approval gate nobody answered — comes back as the non-store value
// "pending" with the approval named, rather than as a job state.
func TestAPIHostRequestWaitDeadlineReturnsPending(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	pingRule(t, h, "ping", "echo ok; exit 0", true)
	token, _, err := MintToken(context.Background(), h.store, "host-gated", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}

	code, data, body := postHostRequest(t, ts.URL, token, "ping", "?wait=true&timeout=100ms", nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", code, body)
	}
	if data.JobState != "pending" {
		t.Fatalf("job_state = %q, want pending (%s)", data.JobState, body)
	}
	if data.JobID == nil || data.ApprovalID == nil {
		t.Fatalf("job_id = %v approval_id = %v, want both named (%s)", data.JobID, data.ApprovalID, body)
	}
}

// TestAPIHostRequestRetryWaitsOnOriginalJob: poll-without-enumeration. A retry
// with the same Idempotency-Key is suppressed onto the original job, and
// ?wait=true on that retry waits on that original job, not a new one.
func TestAPIHostRequestRetryWaitsOnOriginalJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	pingRule(t, h, "ping", "sleep 1; exit 0", false)
	token, _, err := MintToken(context.Background(), h.store, "host-retry", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string]string{"Idempotency-Key": "retry-key-1"}

	code, first, body := postHostRequest(t, ts.URL, token, "ping", "", hdr)
	if code != http.StatusAccepted || first.JobID == nil {
		t.Fatalf("first request: status %d job_id %v (%s)", code, first.JobID, body)
	}
	if first.Outcome != "fired" {
		t.Fatalf("first outcome = %q, want fired (%s)", first.Outcome, body)
	}

	code, retry, body := postHostRequest(t, ts.URL, token, "ping", "?wait=true&timeout=10s", hdr)
	if code != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202 (%s)", code, body)
	}
	if retry.JobID == nil || *retry.JobID != *first.JobID {
		t.Fatalf("retry job_id = %v, want the original %v (%s)", retry.JobID, first.JobID, body)
	}
	if retry.JobState != string(store.StateSucceeded) {
		t.Fatalf("retry job_state = %q, want %q — it waited on the original job (%s)",
			retry.JobState, store.StateSucceeded, body)
	}
}

// TestAPIHostRequestWaitTimeoutCapped: a ?timeout= beyond the maximum is
// refused with a 400 naming it, never silently clamped or accepted.
func TestAPIHostRequestWaitTimeoutCapped(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	pingRule(t, h, "ping", "echo ok; exit 0", false)
	token, _, err := MintToken(context.Background(), h.store, "host-cap", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}

	code, _, body := postHostRequest(t, ts.URL, token, "ping", "?wait=true&timeout=999h", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != codeInvalidArgument {
		t.Fatalf("envelope = %s, want error/%s", body, codeInvalidArgument)
	}
	if !strings.Contains(body, hostRequestMaxWait.String()) {
		t.Fatalf("400 does not name the maximum %s: %s", hostRequestMaxWait, body)
	}
}

func TestAPIHostRequestRetryReturnsOriginalJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	if _, err := h.store.CreateRule(context.Background(), store.NewRule{
		Name:         "host-ping2",
		SourceFilter: reactive.SourceHost,
		CELExpr:      `event.name == "ping"`,
		ActionKind:   store.ActionShell,
		Action:       store.RuleAction{Argv: []string{"/bin/sh", "-c", "echo ok; exit 0"}},
	}); err != nil {
		t.Fatal(err)
	}
	token, _, err := MintToken(context.Background(), h.store, "host-node4", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}

	key := "retry-key"
	code, _, body := bearerPost(t, ts.URL, "/api/v1/hosts/self/requests", token,
		`{"request":"ping"}`, map[string]string{"Idempotency-Key": key})
	if code != http.StatusAccepted {
		t.Fatalf("first status = %d (%s)", code, body)
	}
	var first struct {
		Data struct {
			JobID *int64 `json:"job_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &first)
	if first.Data.JobID == nil {
		t.Fatal("first job_id missing")
	}

	code2, _, body2 := bearerPost(t, ts.URL, "/api/v1/hosts/self/requests", token,
		`{"request":"ping"}`, map[string]string{"Idempotency-Key": key})
	if code2 != http.StatusAccepted {
		t.Fatalf("retry status = %d (%s)", code2, body2)
	}
	if !strings.Contains(body2, `"suppressed"`) {
		t.Fatalf("retry outcome not suppressed: %s", body2)
	}
	var second struct {
		Data struct {
			JobID *int64 `json:"job_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body2), &second)
	if second.Data.JobID == nil || *second.Data.JobID != *first.Data.JobID {
		t.Fatalf("retry job_id = %v, want original %v", second.Data.JobID, first.Data.JobID)
	}
}

func TestAPIHostRequestWait(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	if _, err := h.store.CreateRule(context.Background(), store.NewRule{
		Name:         "host-ping3",
		SourceFilter: reactive.SourceHost,
		CELExpr:      `event.name == "ping"`,
		ActionKind:   store.ActionShell,
		Action:       store.RuleAction{Argv: []string{"/bin/sh", "-c", "echo ok; exit 0"}},
	}); err != nil {
		t.Fatal(err)
	}
	token, _, err := MintToken(context.Background(), h.store, "host-node5", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}

	code, _, body := bearerPost(t, ts.URL, "/api/v1/hosts/self/requests?wait=true&timeout=5s", token,
		`{"request":"ping"}`, nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d (%s)", code, body)
	}
	if !strings.Contains(body, `"job_state"`) || !strings.Contains(body, `"succeeded"`) {
		t.Fatalf("missing succeeded job_state: %s", body)
	}
}
