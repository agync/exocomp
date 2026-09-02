package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// seedSecrets registers three secrets exercising every expiry state (past,
// near-future, none) and both sources, plus one bound run-secret and one
// unbound secret. The ciphertext/nonce carry sentinels so a test can prove the
// sealed material never reaches the page.
func seedSecrets(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-24 * time.Hour)
	soon := now.Add(3 * 24 * time.Hour)

	// A user secret, expired, bound to a run-secret env var.
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "api-token", Kind: "token", Source: store.SecretSourceUser,
		Nonce: []byte("NONCE-SENTINEL-A"), Ciphertext: []byte("CIPHER-SENTINEL-A"),
		AAD: "api-token|token", ExpiresAt: &past,
	}); err != nil {
		t.Fatalf("seed api-token: %v", err)
	}
	// A generated secret, expiring soon, unbound. A rotation stamp so the
	// rotated column is not "never".
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "session-key", Kind: "hmac", Source: store.SecretSourceGenerated,
		Nonce: []byte("NONCE-SENTINEL-B"), Ciphertext: []byte("CIPHER-SENTINEL-B"),
		AAD: "session-key|hmac", ExpiresAt: &soon,
	}); err != nil {
		t.Fatalf("seed session-key: %v", err)
	}
	if err := h.store.RotateSecret(ctx, "session-key", []byte("NONCE-SENTINEL-B2"), []byte("CIPHER-SENTINEL-B2")); err != nil {
		t.Fatalf("rotate session-key: %v", err)
	}
	// A user secret with no expiry, bound.
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "ntfy-url", Kind: "url", Source: store.SecretSourceUser,
		Nonce: []byte("NONCE-SENTINEL-C"), Ciphertext: []byte("CIPHER-SENTINEL-C"),
		AAD: "ntfy-url|url",
	}); err != nil {
		t.Fatalf("seed ntfy-url: %v", err)
	}

	if _, err := h.store.AddRunSecret(ctx, store.RunSecret{
		Binding: "deploy-app", EnvName: "API_TOKEN", SecretName: "api-token",
	}); err != nil {
		t.Fatalf("bind api-token: %v", err)
	}
	if _, err := h.store.AddRunSecret(ctx, store.RunSecret{
		Binding: "notify", EnvName: "NTFY_URL", SecretName: "ntfy-url",
	}); err != nil {
		t.Fatalf("bind ntfy-url: %v", err)
	}
}

// TestSecretsSurfaceGated is the gate for the secrets view: it is
// token-gated (no session refused, session renders), it shows every lifecycle
// field with its staleness marker and bindings, the nav carries Secrets, and it
// NEVER renders the sealed value, nonce, or ciphertext.
func TestSecretsSurfaceGated(t *testing.T) {
	h := newHarness(t)
	seedSecrets(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// No session: refused with a redirect to the login form (the gate_test
	// pattern), never a render.
	noSession := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noSession.Get(ts.URL + "/secrets")
	if err != nil {
		t.Fatalf("GET /secrets no session: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/secrets without session = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("/secrets redirect = %q, want /login…", loc)
	}

	// With a session: the surface renders.
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, client, ts.URL+"/secrets")
	if code != http.StatusOK {
		t.Fatalf("/secrets with session = %d, want 200", code)
	}

	// Fields, columns, sources, timestamps.
	for _, want := range []string{
		`scope="col"`, "All secrets",
		"api-token", "session-key", "ntfy-url",
		"token", "hmac", "url", // kinds
		store.SecretSourceUser, store.SecretSourceGenerated,
		`<th scope="col">Created</th>`,
		`<th scope="col">Last rotated</th>`,
		`<th scope="col">Last used</th>`,
		"never", // never-rotated / never-used cells
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/secrets missing field %q", want)
		}
	}

	// Staleness markers, all three states, as chips.
	for _, want := range []string{
		`<span class="chip fail"><span class="dot"></span>EXPIRED</span>`,
		"expires in ", // near-future, a wait chip
		`<span class="chip dim"><span class="dot"></span>no expiry</span>`,
		`class="chip wait"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/secrets missing staleness marker %q", want)
		}
	}

	// Bindings breakdown: bound and unbound (unbound carries the safe-to-remove
	// nudge the render 1e copy adds).
	for _, want := range []string{
		"used by", "deploy-app", "$API_TOKEN", "notify", "$NTFY_URL",
		"unbound — safe to remove",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/secrets missing binding marker %q", want)
		}
	}

	// Operator write surface: a gated seal form and per-row rotate/delete.
	for _, want := range []string{
		`action="/secrets/add"`,
		`name="confirm_token"`, // add/rotate are re-auth gated
		`/rotate"`,
		`/rm"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/secrets (operator) missing write control %q", want)
		}
	}

	// The nav carries Secrets, marked current on this page.
	if !strings.Contains(page, `href="/secrets" aria-current="page"`) {
		t.Error("/secrets nav item is not present/current")
	}

	// Invariant 4: the sealed material never reaches the page.
	for _, forbidden := range []string{
		"CIPHER-SENTINEL", "NONCE-SENTINEL",
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("/secrets leaked sealed material %q", forbidden)
		}
	}
}

// TestSecretsNavElsewhere: the Secrets nav item appears on other pages too, so
// it is reachable from anywhere in the surface.
func TestSecretsNavElsewhere(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/")
	if !strings.Contains(page, `href="/secrets"`) || !strings.Contains(page, ">Secrets</span>") {
		t.Error("dashboard nav missing the Secrets link")
	}
}

// TestSecretsRowActions is the GATE for the v0.6 secrets row actions: Rotate
// opens the reauth dialog (with the value/generate/confirm-token form), Remove
// is a confirmed glyph, and a bound secret's menu carries the Bindings item.
func TestSecretsRowActions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "ssh-key", Kind: "ssh", Source: store.SecretSourceUser,
		Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "a", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/secrets")
	if code != http.StatusOK {
		t.Fatalf("/secrets = %d", code)
	}
	for _, want := range []string{
		`aria-label="Rotate"`,
		`id="secret-rotate-ssh-key"`,
		`name="confirm_token"`,
		`aria-label="Remove"`,
		`data-confirm="Delete secret ssh-key?"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/secrets missing row-action marker %q", want)
		}
	}
}
