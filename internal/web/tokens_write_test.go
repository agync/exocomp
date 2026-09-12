package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestTokenRevokeRequiresConfirmation: the hidden confirmed=1 field is the
// server-side gate a revoke POST must carry — a session alone must not be able
// to destroy a token. A confirmed-less revoke re-renders the surface at 422
// with a human-copy status line and the token survives; with confirmed=1 it
// lands as a 303 + flash.

func TestTokenRevokeRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)
	_ = h.mint(t, "victim", "readonly")

	resp := postForm(t, client, ts.URL+"/tokens/victim/revoke", url.Values{}, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("revoke without confirmed = %d, want 422 (%s)", resp.StatusCode, string(body))
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("refusal redirected to %q; a refusal must render on the surface", loc)
	}
	for _, want := range []string{
		"Confirm the revoke to take it down.",
		`role="status" aria-live="polite"`,
		`data-refusal-status`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("revoke refusal missing %q", want)
		}
	}
	if _, err := h.store.GetTokenByName(ctx, "victim"); err != nil {
		t.Fatalf("token vanished despite refused revoke: %v", err)
	}

	resp = postForm(t, client, ts.URL+"/tokens/victim/revoke", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke with confirmed = %d, want 303", resp.StatusCode)
	}
	revoked, err := h.store.GetTokenByName(ctx, "victim")
	if err != nil {
		t.Fatalf("GetTokenByName after revoke: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("token not revoked after confirmed revoke")
	}
}

// TestTokenRevokeSelfRefusesOnSurface: a session cannot revoke the token it is
// authenticated with; the refusal re-renders the surface at 422 with the human
// sentence on the status line, and the token survives.
func TestTokenRevokeSelfRefusesOnSurface(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/tokens/abn/revoke", url.Values{"confirmed": {"1"}}, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("self revoke = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), "That&#39;s the token this session is bound to") {
		t.Errorf("self revoke refusal missing the human sentence")
	}
	if _, err := h.store.GetTokenByName(ctx, "abn"); err != nil {
		t.Fatalf("self token vanished despite refused revoke: %v", err)
	}
	if _, err := h.server.authenticateToken(ctx, op, h.server.now().UTC()); err != nil {
		t.Fatalf("session token no longer authenticates after refused self-revoke: %v", err)
	}
}
