package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postToken sends a tokens write POST and returns status + body without
// following a redirect, for the 422-render assertions.
func postToken(t *testing.T, client *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	resp := postForm(t, client, target, form, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body)
}

// TestTokenNewFormAtRest renders /tokens for an operator and asserts the mint
// dialog's forms-round anatomy: fieldInput controls with always-present meta
// lines, the scope select with its own meta, a form-status line above the
// primary, and novalidate so the daemon stays authoritative.
func TestTokenNewFormAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/tokens")
	if code != http.StatusOK {
		t.Fatalf("/tokens = %d, want 200", code)
	}
	for _, want := range []string{
		`id="tokens-mint"`,
		`action="/tokens/new"`,
		`id="tokens-new-form" novalidate`,
		`aria-describedby="tk-name-meta"`,
		`aria-describedby="tk-exp-meta"`,
		`id="tk-name-meta"`,
		`id="tk-exp-meta"`,
		"Letters, digits, dots, hyphens, underscores. Blank mints a device-pairing token.",
		"A duration — e.g. 720h, 30d.",
		"What the token may do — operator, readonly, or approve.",
		`role="status" aria-live="polite"`,
		"Mint token",
		`id="tk-scope" name="scope"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("mint form missing %q", want)
		}
	}
	if !strings.Contains(page, `placeholder="blank to auto-generate" aria-describedby="tk-name-meta">`) {
		t.Error("at-rest name control carries an aria-invalid refusal")
	}
}

// TestTokenNewRefusalRenders: a 422 re-render keeps the non-secret typed values
// (name, scope, expires) with ▲ human-copy refusals and the mint dialog
// re-opened, but never re-renders the confirm token (invariant 4).
func TestTokenNewRefusalRenders(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)

	code, page := postToken(t, op, ts.URL+"/tokens/new",
		url.Values{"name": {"Bad Name"}, "scope": {"readonly"}, "expires": {"nope"},
			"confirm_token": {tok}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("refusal status = %d, want 422", code)
	}
	for _, want := range []string{
		`id="tk-name"`, `value="Bad Name"`, "Names can&#39;t have spaces.",
		`value="nope"`, "That&#39;s not a duration — like 720h or 30d.",
		`value="readonly" selected`, ` aria-invalid aria-describedby="tk-name-meta"`, `exoModalOpen("tokens-mint")`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("mint 422 missing %q", want)
		}
	}
	if strings.Contains(page, tok) {
		t.Error("mint 422 re-rendered the confirm token")
	}
}

// TestTokenNewDuplicateRefuses: a duplicate name lands on the name field's meta
// line at 422, and the confirm token still never re-renders.
func TestTokenNewDuplicateRefuses(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)
	_ = h.mint(t, "dup", "readonly")

	code, page := postToken(t, op, ts.URL+"/tokens/new",
		url.Values{"name": {"dup"}, "scope": {"operator"}, "confirm_token": {tok}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate status = %d, want 422", code)
	}
	if !strings.Contains(page, "A token called dup already exists.") {
		t.Errorf("duplicate 422 missing the name-field sentence")
	}
	if strings.Contains(page, tok) {
		t.Error("duplicate 422 re-rendered the confirm token")
	}
}

// TestTokenRotateFormAtRest renders the rotate dialog for another operator's
// token and asserts the field anatomy: an expires control with its meta line
// and a status line.
func TestTokenRotateFormAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_ = h.mint(t, "other", "operator")

	code, page := getBody(t, op, ts.URL+"/tokens")
	if code != http.StatusOK {
		t.Fatalf("/tokens = %d, want 200", code)
	}
	for _, want := range []string{
		`id="token-rotate-form-other" novalidate`,
		`aria-describedby="tk-rot-exp-other-meta"`,
		`id="tk-rot-exp-other-meta"`,
		"A duration — e.g. 720h, 30d.",
		`role="status" aria-live="polite"`,
		"Rotate",
		`name="confirm_token"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("token rotate form missing %q", want)
		}
	}
}

// TestTokenRotateRefusalKeepsExpires: a rotate 422 keeps the typed expires
// value and re-opens the right dialog, but never re-renders the confirm token.
func TestTokenRotateRefusalKeepsExpires(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)
	_ = h.mint(t, "other", "operator")

	code, page := postToken(t, op, ts.URL+"/tokens/other/rotate",
		url.Values{"expires": {"nope"}, "confirm_token": {tok}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("rotate refusal status = %d, want 422", code)
	}
	for _, want := range []string{
		`value="nope"`, "That&#39;s not a duration — like 720h or 30d.",
		`data-modal="tk-rotate-other"`,
		`id="tk-rot-exp-other-meta"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("token rotate 422 missing %q", want)
		}
	}
	if strings.Contains(page, tok) {
		t.Error("token rotate 422 re-rendered the confirm token")
	}
}
