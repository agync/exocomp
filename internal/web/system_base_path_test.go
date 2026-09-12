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

// The /system public-url write must not accept a URL the reactive ingress
// cannot answer: decision links are rooted at baseURL + "/approval/…"
// (internal/reactive/token.go) and the ingress serves /hook and /approval at
// the ORIGIN ROOT with no path support (internal/reactive/ingress.go), so a
// path-bearing public-url would point every decision link at a path nothing
// serves. The page also shows the effective base path the surface is served
// under ("" at the origin root, "/exocomp" under --base-path /exocomp).

func TestPublicURLRefusesAPath(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if err := h.store.PutSetting(ctx, store.SettingPublicURL, "https://exocomp.example.ts.net"); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/system/public-url",
		url.Values{"url": {"https://exocomp.example.ts.net/exocomp"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("path-bearing public url = %d, want 400 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "origin root") {
		t.Errorf("path refusal copy missing the origin-root reason: %s", body)
	}
	if strings.Contains(body, "invalid") {
		t.Error("path refusal copy uses the word 'invalid'")
	}
	if got, _ := h.store.GetSetting(ctx, store.SettingPublicURL); got != "https://exocomp.example.ts.net" {
		t.Fatalf("path-bearing url overwrote the setting: %q", got)
	}
}

func TestPublicURLPathlessStillSaves(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/system/public-url",
		url.Values{"url": {"https://exocomp.example.ts.net"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("pathless public url = %d, want 303", resp.StatusCode)
	}
	if got, err := h.store.GetSetting(ctx, store.SettingPublicURL); err != nil || got != "https://exocomp.example.ts.net" {
		t.Fatalf("pathless public url = %q, want the posted value", got)
	}
}

func TestSystemShellBasePrefixed(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "abn", "operator")
	client := baseLogin(t, ts.URL, "/exocomp", op)

	code, body := getBody(t, client, ts.URL+"/exocomp/system")
	if code != http.StatusOK {
		t.Fatalf("GET /exocomp/system = %d, want 200", code)
	}
	for _, want := range []string{
		`href="/exocomp/system#deadman"`,
		`href="/exocomp/system#ntfy"`,
		`href="/exocomp/system#litestream"`,
		`href="/exocomp/system#backups"`,
		`href="/exocomp/system/deadman"`,
		`href="/exocomp/system/ntfy"`,
		`href="/exocomp/system/litestream"`,
		`href="/exocomp/system/backups"`,
		`action="/exocomp/system/public-url"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("system shell missing the prefixed %q", want)
		}
	}
	if strings.Contains(body, `href="/system#deadman"`) {
		t.Error("system shell anchor not prefixed under --base-path")
	}
}

func TestSystemShellOriginRoot(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)

	code, body := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("GET /system = %d, want 200", code)
	}
	if !strings.Contains(body, `href="/system#deadman"`) {
		t.Error("system shell anchor missing at the origin root")
	}
	if strings.Contains(body, "/exocomp/system") {
		t.Error("origin-root system shell carries a phantom prefix")
	}
}
