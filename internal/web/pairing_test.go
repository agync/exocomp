package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestMintBlankNameAutoGeneratesAndRendersQR: a blank device name auto-mints a
// unique device-<hex> token, and the result page carries the one-time secret in
// a copy block plus the pairing QR.
func TestMintBlankNameAutoGeneratesAndRendersQR(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/tokens/new",
		url.Values{"name": {""}, "scope": {"operator"}, "confirm_token": {op}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `class="pairqr"`) || !strings.Contains(body, "<svg") || strings.Count(body, "<rect") < 10 {
		t.Fatalf("result page missing pairing QR: %s", body)
	}
	// The secret shows once inside the copy block (codeblock => secretbox code).
	if !strings.Contains(body, "secretbox code") || !strings.Contains(body, "copybtn") {
		t.Fatalf("result page missing token copy block: %s", body)
	}

	// A new token was created under an auto-generated device-<hex> name.
	rows, err := h.store.ListTokens(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	var found string
	for _, r := range rows {
		if strings.HasPrefix(r.Name, "device-") {
			found = r.Name
		}
	}
	if found == "" {
		t.Fatalf("no auto-generated device token; rows=%+v", rows)
	}
	if !strings.Contains(body, found) {
		t.Fatalf("result page does not name the minted device %q", found)
	}
}

func TestSetPublicURL(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)

	// A good absolute URL is accepted and persisted.
	resp := postForm(t, client, ts.URL+"/system/public-url",
		url.Values{"url": {"https://exocomp.example.ts.net"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("set public url = %d, want 303", resp.StatusCode)
	}
	got, err := h.store.GetSetting(ctx, store.SettingPublicURL)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "https://exocomp.example.ts.net" {
		t.Fatalf("public url = %q, want the posted value", got)
	}

	// A non-absolute / non-http URL is rejected and does not overwrite: the
	// refusal re-renders /system at 422 with the field's ▲ meta line (A7) —
	// never a flash.
	resp = postForm(t, client, ts.URL+"/system/public-url",
		url.Values{"url": {"not-a-url"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad url = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `id="pub-url-meta" class="meta is-error"`) ||
		!strings.Contains(body, "Give the public URL as a full address") {
		t.Errorf("bad url refusal missing the field's is-error meta:\n%s", body)
	}
	if !strings.Contains(body, `value="not-a-url"`) {
		t.Errorf("bad url refusal lost the typed value:\n%s", body)
	}
	if got, _ := h.store.GetSetting(ctx, store.SettingPublicURL); got != "https://exocomp.example.ts.net" {
		t.Fatalf("bad url overwrote the setting: %q", got)
	}
}

func TestSetPublicURLReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/system/public-url",
		url.Values{"url": {"https://h.example"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly set public url = %d, want 403", resp.StatusCode)
	}
}
