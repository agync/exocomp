package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestSecretRmRequiresConfirmation: the hidden confirmed=1 field is the
// server-side gate the secrets Delete form's dialog re-submission carries —
// a session alone must not be able to destroy a sealed secret. A
// confirmed-less rm reads 400 and the secret survives; with confirmed=1
// it lands.

func TestSecretRmRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "ssh-key", Kind: "generic", Source: store.SecretSourceUser,
		Nonce: []byte("NONCE-SENTINEL-D"), Ciphertext: []byte("CIPHER-SENTINEL-D"),
		AAD: "ssh-key|generic",
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/secrets/ssh-key/rm", url.Values{}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rm without confirmed = %d, want 400 (%s)", resp.StatusCode, body)
	}
	if _, err := h.store.GetSecret(ctx, "ssh-key"); err != nil {
		t.Fatalf("secret vanished despite refused rm: %v", err)
	}

	resp = postForm(t, client, ts.URL+"/secrets/ssh-key/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm with confirmed = %d, want 303", resp.StatusCode)

	}
	if _, err := h.store.GetSecret(ctx, "ssh-key"); err == nil {
		t.Fatal("secret still present after confirmed rm")
	}
}
