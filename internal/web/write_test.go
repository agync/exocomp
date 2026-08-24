package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postForm sends a cookie-session form POST with the client's jar, without
// following redirects, so a 303/403/503/200 is observed directly. extra headers
// (e.g. Sec-Fetch-Site) override the browser-default same-origin assumption.
func postForm(t *testing.T, client *http.Client, target string, form url.Values, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	prev := client.CheckRedirect
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { client.CheckRedirect = prev }()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	return resp
}

func TestWriteReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/tokens/new",
		url.Values{"name": {"x"}, "scope": {"readonly"}, "confirm_token": {ro}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly write = %d, want 403", resp.StatusCode)
	}
}

func TestWriteCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/tokens/new",
		url.Values{"name": {"x"}, "confirm_token": {op}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin write = %d, want 403", resp.StatusCode)
	}
}

func TestTokenMintGatedByReauth(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	// Wrong confirm_token: the write is refused and nothing is minted.
	bad := postForm(t, client, ts.URL+"/tokens/new",
		url.Values{"name": {"minted"}, "scope": {"readonly"}, "confirm_token": {"exo_9_wrongsecret"}}, nil)
	_ = bad.Body.Close()
	if _, err := h.store.GetTokenByName(ctx, "minted"); err == nil {
		t.Fatal("a token was minted despite a bad confirm_token")
	}

	// Correct confirm_token (the session's own token): minted, shown once.
	ok := postForm(t, client, ts.URL+"/tokens/new",
		url.Values{"name": {"minted"}, "scope": {"readonly"}, "confirm_token": {op}}, nil)
	body := readBody(t, ok)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("gated mint = %d, want 200", ok.StatusCode)
	}
	if !strings.Contains(body, "exo_") {
		t.Fatal("mint result did not show the new secret once")
	}
	if _, err := h.store.GetTokenByName(ctx, "minted"); err != nil {
		t.Fatalf("token not created after valid re-auth: %v", err)
	}
}

func TestSecretAddSeals(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/secrets/add",
		url.Values{"name": {"proxmox/token"}, "kind": {"generic"}, "value": {"s3cr3t"}, "confirm_token": {op}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("secret add = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.GetSecret(context.Background(), "proxmox/token"); err != nil {
		t.Fatalf("secret not sealed: %v", err)
	}
}

// (readBody is defined in kill_test.go)

func TestSecretAddNoSealer503(t *testing.T) {
	h := newHarness(t)
	// A server with no Sealer (a web-only process holding no envelope).
	noseal, err := New(Options{Store: h.store, Deriver: h.env, Version: "test", DataDir: h.dir})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(noseal.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/secrets/add",
		url.Values{"name": {"x"}, "value": {"y"}, "confirm_token": {op}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("secret add with nil Sealer = %d, want 503", resp.StatusCode)
	}
}
