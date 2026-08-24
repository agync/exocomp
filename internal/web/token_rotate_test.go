package web

import (
	"context"
	"net/http"
	"net/http/httptest"
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
