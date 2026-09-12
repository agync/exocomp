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

	// A7: the validPublicURL guard's refusal is a field refusal — it re-renders
	// /system at 422 with the field's ▲ meta line, the typed value kept, and no
	// flash (forms/SPEC.md: flash is success-only). A 303 redirect would be a
	// flash and this test would red.
	resp := postForm(t, client, ts.URL+"/system/public-url",
		url.Values{"url": {"https://exocomp.example.ts.net/exocomp"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("path-bearing public url = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `id="pub-url-meta" class="meta is-error"`) {
		t.Errorf("path refusal missing the field's is-error meta: %s", body)
	}
	// The words table (forms/SPEC.md) is binding: the refusal is one sentence
	// naming the origin-root reason, never "invalid"/"error" or a status code.
	// (The page's aria-invalid attribute carries the word "invalid" — the check
	// is on the copy.)
	refusal := "public URL must be at the origin root — the ingress serves /hook and /approval links there, so a path would point them at nothing"
	if !strings.Contains(body, refusal) {
		t.Errorf("path refusal copy missing the origin-root reason: %s", body)
	}
	if !strings.Contains(body, `value="https://exocomp.example.ts.net/exocomp"`) {
		t.Errorf("path refusal lost the typed value on the re-render: %s", body)
	}
	if got, _ := h.store.GetSetting(ctx, store.SettingPublicURL); got != "https://exocomp.example.ts.net" {
		t.Fatalf("path-bearing url overwrote the setting: %q", got)
	}
}

// TestPublicURLMetaAtRest pins the forms anatomy on the Change form at rest:
// the field's meta line always renders its help (approval links and the kit
// use this), the 20px form status line always renders, and neither carries a
// refusal. Dropping the meta line reds this test (mutation).
func TestPublicURLMetaAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if err := h.store.PutSetting(ctx, store.SettingPublicURL, "https://exocomp.example.ts.net"); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		`id="pub-url-meta"`,
		"approval links and the kit use this",
		`aria-describedby="pub-url-meta"`,
		`class="form-status" role="status" aria-live="polite"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Change form at rest missing %q", want)
		}
	}
	if strings.Contains(page, `class="meta is-error"`) {
		t.Error("Change form at rest must not carry a field refusal")
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
