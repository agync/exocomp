package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// getNoRedirect returns the status of a GET without following redirects, so an
// authenticated 200 is distinguishable from an unauthenticated redirect to the
// login form.
func getNoRedirect(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	prev := client.CheckRedirect
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { client.CheckRedirect = prev }()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestRotateDropsOldSessionsAndSecret(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	oldSecret := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, oldSecret)

	// The bootstrapped session reaches the dashboard.
	if code := getNoRedirect(t, client, ts.URL+"/"); code != http.StatusOK {
		t.Fatalf("pre-rotate dashboard = %d, want 200", code)
	}

	newSecret, row, err := RotateToken(ctx, h.store, "abn", 0, false)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if row.Name != "abn" {
		t.Fatalf("rotated token name = %q, want abn", row.Name)
	}

	// The session bootstrapped from the old secret is dropped: the browser is
	// no longer authenticated and is redirected to the login form.
	if code := getNoRedirect(t, client, ts.URL+"/"); code == http.StatusOK {
		t.Fatal("old session still authenticated after rotate")
	}

	// The old bearer secret no longer authenticates; the new one does.
	if _, err := h.server.authenticateToken(ctx, oldSecret, time.Now().UTC()); err == nil {
		t.Fatal("old secret still authenticates after rotate")
	}
	got, err := h.server.authenticateToken(ctx, newSecret, time.Now().UTC())
	if err != nil {
		t.Fatalf("new secret must authenticate: %v", err)
	}
	if got.ID != row.ID {
		t.Fatalf("new secret resolved to token %d, want %d", got.ID, row.ID)
	}
}

// TestRotateSelfRefused: mirrors the revoke self-guard — a session cannot
// rotate the token it is authenticated with, since rotation invalidates the
// secret currently authenticating the request. The refusal re-renders the
// tokens surface at 422 with the human sentence on the rotate dialog's status
// line.
func TestRotateSelfRefused(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)

	before, err := h.store.GetTokenByName(ctx, "abn")
	if err != nil {
		t.Fatalf("GetTokenByName: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/tokens/abn/rotate",
		url.Values{"confirm_token": {op}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("self rotate = %d, want 422; body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"That&#39;s the token this session is bound to",
		`role="status" aria-live="polite"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("self rotate refusal missing %q", want)
		}
	}

	after, err := h.store.GetTokenByName(ctx, "abn")
	if err != nil {
		t.Fatalf("GetTokenByName after: %v", err)
	}
	if after.Prefix != before.Prefix || after.Hash != before.Hash {
		t.Fatal("self token was rotated despite the self-guard")
	}

	// The session's own token still authenticates: the refusal did not
	// consume the reauth path or otherwise disturb the token.
	if _, err := h.server.authenticateToken(ctx, op, time.Now().UTC()); err != nil {
		t.Fatalf("session token no longer authenticates after refused self-rotate: %v", err)
	}
}

// TestTokensPageSuppressesRotateForSelf: the current-session row must not
// offer Rotate or Revoke, while another operator's row still offers both.
func TestTokensPageSuppressesRotateForSelf(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "abn", "operator")
	_ = h.mint(t, "other", "operator")
	client := login(t, ts.URL, op)

	resp, err := client.Get(ts.URL + "/tokens")
	if err != nil {
		t.Fatalf("GET /tokens: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tokens = %d, want 200", resp.StatusCode)
	}

	selfRow := rowFor(t, body, "abn")
	if strings.Contains(selfRow, "Rotate") || strings.Contains(selfRow, "Revoke") {
		t.Fatalf("self row still offers Rotate/Revoke: %s", selfRow)
	}
	if !strings.Contains(selfRow, "this session") {
		t.Fatalf("self row missing 'this session' marker: %s", selfRow)
	}

	otherRow := rowFor(t, body, "other")
	if !strings.Contains(otherRow, "Rotate") || !strings.Contains(otherRow, "Revoke") {
		t.Fatalf("other row missing Rotate/Revoke controls: %s", otherRow)
	}
}

// rowFor extracts the <tr>...</tr> segment naming the given token on a
// rendered tokens page.
func rowFor(t *testing.T, body, name string) string {
	t.Helper()
	marker := `data-label="Name">` + name + `</td>`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("token %q not found in page", name)
	}
	start := strings.LastIndex(body[:i], "<tr>")
	end := strings.Index(body[i:], "</tr>")
	if start < 0 || end < 0 {
		t.Fatalf("could not bound row for %q", name)
	}
	return body[start : i+end]
}
