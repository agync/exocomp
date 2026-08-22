package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// login exchanges a token for a session on a fresh cookie-jar client. It
// follows the post-login redirect so the returned client is authenticated.
func login(t *testing.T, base, token string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.PostForm(base+"/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	if len(jar.Cookies(mustURL(t, base))) == 0 {
		t.Fatal("login set no session cookie")
	}
	return client
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u
}

func getBody(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestSessionCookieAttributes checks the cookie is HttpOnly and SameSite=Strict
// (ADR 0009 §3): the phone surface never wants a cross-site cookie and script
// never needs to read it.
func TestSessionCookieAttributes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "phone", "operator")

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(ts.URL+"/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatal("no session cookie set")
	}
	if !found.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if found.SameSite != http.SameSiteStrictMode {
		t.Error("session cookie is not SameSite=Strict")
	}
}

// TestUnauthenticatedRedirects: a browser with no session is sent to the login
// form, and an API caller with no bearer gets 401.
func TestUnauthenticatedRedirects(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated browser status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("redirect location = %q, want /login…", loc)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/jobs", nil)
	req.Header.Set("Accept", "application/json")
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /jobs json: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status = %d, want 401", resp2.StatusCode)
	}
}

// TestRevokedTokenRejected: a revoked token neither authenticates a bearer
// request nor keeps its session alive (ADR 0009 gate: "session dies with its
// token").
func TestRevokedTokenRejected(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "phone", "operator")

	// A live session first.
	client := login(t, ts.URL, token)
	if code, _ := getBody(t, client, ts.URL+"/"); code != http.StatusOK {
		t.Fatalf("authenticated dashboard status = %d", code)
	}

	// Revoke the token behind the session.
	if err := h.store.RevokeToken(context.Background(), "phone"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// The session is now dead: the browser is redirected to login.
	noFollow := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noFollow.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / after revoke: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoked session status = %d, want 303 to login", resp.StatusCode)
	}

	// And the bearer token is rejected outright.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp2, err := noFollow.Do(req)
	if err != nil {
		t.Fatalf("bearer after revoke: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked bearer status = %d, want 401", resp2.StatusCode)
	}
}

// TestApproveFromPhoneRunsJob is the milestone gate: a gated job, approved
// through the phone surface with a session cookie, runs.
func TestApproveFromPhoneRunsJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "phone", "operator")
	client := login(t, ts.URL, token)

	job, approval := h.gatedJob(t, 30*time.Minute)

	// The gated job is parked and claimable by nothing until approved.
	parked, _ := h.store.GetJob(context.Background(), job.ID)
	if parked.State != store.StateAwaitingApproval {
		t.Fatalf("gated job state = %s, want awaiting_approval", parked.State)
	}

	// The phone approval surface renders the contract.
	code, page := getBody(t, client, ts.URL+"/approvals/"+itoa64(approval.ID))
	if code != http.StatusOK {
		t.Fatalf("approval surface status = %d", code)
	}
	for _, want := range []string{"Auto-rejects in", "class=\"phone\"", "Approve", "Reject", "no rule can approve its own apply"} {
		if !strings.Contains(page, want) {
			t.Errorf("approval surface missing %q", want)
		}
	}

	// Approve it through the session.
	resp, err := client.PostForm(ts.URL+"/approvals/"+itoa64(approval.ID)+"/decide",
		url.Values{"decision": {"approve"}})
	if err != nil {
		t.Fatalf("POST decide: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decide status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "approved") {
		t.Errorf("decision page missing the approved notice:\n%s", string(body))
	}

	// The approval is recorded approved, and the job runs to success.
	decided, _ := h.store.GetApproval(context.Background(), approval.ID)
	if decided.State != store.ApprovalApproved {
		t.Fatalf("approval state = %s, want approved", decided.State)
	}
	if decided.DecidedVia != "web:phone" {
		t.Fatalf("approval decided via %q, want web:phone", decided.DecidedVia)
	}
	ran := h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)
	if ran.ExitCode == nil || *ran.ExitCode != 0 {
		t.Fatalf("approved job did not exit 0: exit=%v", ran.ExitCode)
	}
}

// TestReadonlyCannotApprove: a readonly token can view the surface but its
// decide POST is refused.
func TestReadonlyCannotApprove(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "viewer", "readonly")
	client := login(t, ts.URL, token)

	_, approval := h.gatedJob(t, 30*time.Minute)

	code, page := getBody(t, client, ts.URL+"/approvals/"+itoa64(approval.ID))
	if code != http.StatusOK {
		t.Fatalf("readonly view status = %d", code)
	}
	if !strings.Contains(page, "read-only") {
		t.Error("readonly surface should say the token cannot decide")
	}

	resp, err := client.PostForm(ts.URL+"/approvals/"+itoa64(approval.ID)+"/decide",
		url.Values{"decision": {"approve"}})
	if err != nil {
		t.Fatalf("POST decide: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly decide status = %d, want 403", resp.StatusCode)
	}
}

// TestStalePlanRefused: when the workspace serial moved after the plan, the
// phone shows a refusal with re-plan/discard, not an Approve button.
func TestStalePlanRefused(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	token := h.mint(t, "phone", "operator")
	client := login(t, ts.URL, token)

	ws, err := h.store.CreateWorkspace(ctx, store.Workspace{Name: "prod", TofuDir: "prod"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	job, err := h.queue.EnqueueGated(ctx, queue.Spec{
		Runbook: "apply-prod", Kind: "tofu", Trigger: "drift",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("gated: %v", err)
	}
	_, hash, _ := reactive.ApprovalToken(h.env, job.ID)
	now := time.Now().UTC()
	approval, err := h.store.CreateApproval(ctx, store.Approval{
		JobID: job.ID, Workspace: "prod", PlanSerial: 1,
		TokenHash: hash, RequestedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	// Move the workspace past the plan's serial via a state push.
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 3, Lineage: "l", Ciphertext: []byte("x"),
	}); err != nil {
		t.Fatalf("append tfstate: %v", err)
	}

	code, page := getBody(t, client, ts.URL+"/approvals/"+itoa64(approval.ID))
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(page, "Plan is stale") {
		t.Errorf("stale plan surface missing the refusal:\n%s", page)
	}
	if strings.Contains(page, ">Approve<") {
		t.Error("a stale plan must not offer Approve")
	}
}
