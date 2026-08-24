package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// TestAPIWriteRejectsCookieAuth proves the machine API cannot be driven by an
// ambient browser session: it used to accept the browser's ambient session on
// every POST, with no CSRF token, no Origin check and no Content-Type check —
// so any same-site page (another app on another port, an XSS, a local page
// under --insecure-http) could drive operator-scope writes with the cookie
// the browser attaches for it. /api/v1 writes now require an explicit bearer
// credential.
func TestAPIWriteRejectsCookieAuth(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	h.seedWorkspace(t, "web")

	token := h.mint(t, "op", "operator")
	client := login(t, ts.URL, token)

	// The exploit shape: cookie only, text/plain (a CORS "simple request", so no
	// preflight), foreign Origin, empty body — the destructive case.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/workspaces/web/apply",
		strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("cookie apply: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie-authenticated apply status = %d, want 403", resp.StatusCode)
	}
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{Runbook: service.WorkspaceRunbook("web")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("an ambient-cookie write enqueued %d job(s)", len(jobs))
	}

	// The same token as a bearer still writes: the guard is the credential's
	// kind, not the token.
	if code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/apply", token, "", nil); code != http.StatusOK {
		t.Fatalf("bearer apply status = %d, want 200 (%s)", code, body)
	}

	// Reads keep working from a session: the browser surface is built on them.
	code, _ := getBody(t, client, ts.URL+"/api/v1/jobs")
	if code != http.StatusOK {
		t.Fatalf("cookie-authenticated read status = %d, want 200", code)
	}
}
