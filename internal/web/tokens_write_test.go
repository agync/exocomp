package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestTokenRevokeRequiresConfirmation: the hidden confirmed=1 field is the
// server-side gate a revoke POST must carry — a session alone must not be
// able to destroy a token. A confirmed-less revoke reads 400 and the token
// survives; the UI dialog then re-submits with confirmed=1 and it lands.

func TestTokenRevokeRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)
	_ = h.mint(t, "victim", "readonly")

	resp := postForm(t, client, ts.URL+"/tokens/victim/revoke", url.Values{}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("revoke without confirmed = %d, want 400 (%s)", resp.StatusCode, body)
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
