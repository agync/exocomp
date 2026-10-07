package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	// Fields, columns, sources, timestamps. The inline row is the scan set
	// only — Name · Kind · Source · Expiry · Last used (+ Actions); Created,
	// Last rotated and Used by live in the row's details dialog.
	for _, want := range []string{
		`scope="col"`, "Sealed values",
		"api-token", "session-key", "ntfy-url",
		"token", "hmac", "url", // kinds
		store.SecretSourceUser, store.SecretSourceGenerated,
		`<th scope="col">Name</th>`,
		`<th scope="col">Kind</th>`,
		`<th scope="col">Source</th>`,
		`<th scope="col">Expiry</th>`,
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

// postSecret sends a secrets write POST and returns status + body without
// following a redirect, for the 422-render assertions.
func postSecret(t *testing.T, client *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	resp := postForm(t, client, target, form, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body)
}

// TestSecretRmBoundRefusalOnRow: removing a bound secret re-renders the
// secrets surface at 422 with the refusal on the row — the forms/SPEC.md
// button-verb shape — never a flash, and the secret stays. The row has no
// force control (the CLI's --force stays CLI), so the refusal names the
// binding without offering the override.
func TestSecretRmBoundRefusalOnRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "api-token", Kind: "token", Source: store.SecretSourceUser,
		Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "a", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := h.store.AddRunSecret(ctx, store.RunSecret{
		Binding: "deploy-app", EnvName: "API_TOKEN", SecretName: "api-token",
	}); err != nil {
		t.Fatalf("bind api-token: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	status, body := postSecret(t, op, ts.URL+"/secrets/api-token/rm", url.Values{"confirmed": {"1"}})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("rm refusal status = %d, want 422", status)
	}
	for _, want := range []string{
		`form-status is-error`,
		"Nothing was removed — api-token is still bound as a run secret.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rm refusal missing %q", want)
		}
	}
	if _, err := h.store.GetSecret(ctx, "api-token"); err != nil {
		t.Fatal("refused rm removed the bound secret")
	}
}

// TestSecretRmRefusalOnRow: a confirmed-less remove re-renders the secrets
// surface at 422 with the refusal on the row — never a flash.
func TestSecretRmRefusalOnRow(t *testing.T) {
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

	status, body := postSecret(t, op, ts.URL+"/secrets/ssh-key/rm", url.Values{})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("rm refusal status = %d, want 422", status)
	}
	if !strings.Contains(body, "Nothing was removed — confirm the removal in the dialog.") {
		t.Errorf("rm refusal missing the confirm-gate sentence")
	}
	if _, err := h.store.GetSecret(ctx, "ssh-key"); err != nil {
		t.Fatal("refused rm removed the secret")
	}
}

// TestSecretAddFormAtRest renders /secrets for an operator and asserts the
// seal dialog's forms-round anatomy: every fieldInput control carries an
// always-present meta line (help text at rest), the aria wiring ties each
// control to its meta, a form-status line sits above the primary, and the form
// is novalidate so the daemon stays authoritative. The value field's meta
// states the sealed contract; the control itself always renders empty.
func TestSecretAddFormAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)

	code, page := getBody(t, op, ts.URL+"/secrets")
	if code != http.StatusOK {
		t.Fatalf("/secrets = %d, want 200", code)
	}
	for _, want := range []string{
		`id="secrets-add"`,
		`action="/secrets/add"`,
		`id="secrets-add-form" novalidate`,
		`aria-describedby="se-name-meta"`,
		`aria-describedby="se-kind-meta"`,
		`aria-describedby="se-val-meta"`,
		`aria-describedby="se-exp-meta"`,
		`id="se-name-meta"`,
		`id="se-kind-meta"`,
		`id="se-val-meta"`,
		`id="se-exp-meta"`,
		"Letters, digits, dots, hyphens, underscores, slashes.",
		"Sealed into the envelope — never shown again.",
		"A duration — e.g. 720h, 30d.",
		`role="status" aria-live="polite"`,
		"Seal secret",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("seal form missing %q", want)
		}
	}
	// At rest no control is flagged invalid; the value control renders empty.
	if !strings.Contains(page, `placeholder="deploy-token" aria-describedby="se-name-meta">`) {
		t.Error("at-rest name control carries an aria-invalid refusal")
	}
	if !strings.Contains(page, `id="se-val-meta" class="meta">Sealed into the envelope`) {
		t.Error("at-rest value meta is not plain help text")
	}
	if !strings.Contains(page, `id="se-val" name="value" value=""`) {
		t.Error("value control does not render empty at rest")
	}
}

// TestSecretAddRefusalRenders is the invariant-4 load-bearing assertion: a 422
// re-render keeps the NON-secret typed values (name, kind, expires) with ▲
// human-copy refusals on their meta lines and the add dialog re-opened, but the
// secret VALUE and the confirm_token never re-render (assert the body lacks
// them).
func TestSecretAddRefusalRenders(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)

	code, page := postSecret(t, op, ts.URL+"/secrets/add",
		url.Values{"name": {"Bad Name"}, "kind": {"token"}, "value": {"supersecretvalue"},
			"expires": {"nope"}, "confirm_token": {tok}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("refusal status = %d, want 422", code)
	}
	for _, want := range []string{
		`id="se-name"`, `value="Bad Name"`, "Names can&#39;t have spaces.",
		`value="token"`, `value="nope"`, "That&#39;s not a duration — like 720h or 30d.",
		` aria-invalid aria-describedby="se-name-meta"`, `exoModalOpen("secrets-add")`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("seal 422 missing %q", want)
		}
	}
	// Invariant 4: the sealed value and the confirm token never re-render.
	for _, forbidden := range []string{"supersecretvalue", tok} {
		if strings.Contains(page, forbidden) {
			t.Errorf("seal 422 re-rendered a secret %q", forbidden)
		}
	}
}

// TestSecretAddDuplicateRefuses: a duplicate name lands on the name field's
// meta line at 422, and the sealed value still never re-renders.
func TestSecretAddDuplicateRefuses(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)
	ctx := context.Background()
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "dup", Kind: "generic", Source: store.SecretSourceUser,
		Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "dup|generic",
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	code, page := postSecret(t, op, ts.URL+"/secrets/add",
		url.Values{"name": {"dup"}, "value": {"duplicatesecretvalue"}, "confirm_token": {tok}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate status = %d, want 422", code)
	}
	if !strings.Contains(page, "A secret called dup already exists.") {
		t.Errorf("duplicate 422 missing the name-field sentence")
	}
	if strings.Contains(page, "duplicatesecretvalue") {
		t.Error("duplicate 422 re-rendered the sealed value")
	}
}

// TestSecretRotateFormAtRest renders the rotate dialog for a seeded secret and
// asserts the field anatomy: a never-echoing value control and an expires
// control, each with its always-present meta line and a status line.
func TestSecretRotateFormAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)
	ctx := context.Background()
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "ssh-key", Kind: "ssh", Source: store.SecretSourceUser,
		Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "ssh-key|ssh",
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	code, page := getBody(t, op, ts.URL+"/secrets")
	if code != http.StatusOK {
		t.Fatalf("/secrets = %d, want 200", code)
	}
	for _, want := range []string{
		`id="secret-rotate-form-ssh-key" novalidate`,
		`aria-describedby="sv-ssh-key-meta"`,
		`aria-describedby="se-exp-ssh-key-meta"`,
		`id="sv-ssh-key-meta"`,
		`id="se-exp-ssh-key-meta"`,
		"Sealed into the envelope — never shown again.",
		"A duration — e.g. 720h, 30d.",
		`role="status" aria-live="polite"`,
		"Rotate",
		`name="confirm_token"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rotate form missing %q", want)
		}
	}
}

// TestSecretRotateRefusalKeepsExpiresNotValue: a rotate 422 keeps the typed
// expires value and re-opens the right dialog, but never re-renders the new
// value or the confirm token (invariant 4).
func TestSecretRotateRefusalKeepsExpiresNotValue(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)
	ctx := context.Background()
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "ssh-key", Kind: "ssh", Source: store.SecretSourceUser,
		Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "ssh-key|ssh",
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	code, page := postSecret(t, op, ts.URL+"/secrets/ssh-key/rotate",
		url.Values{"value": {"rotatedsecretvalue"}, "expires": {"nope"}, "confirm_token": {tok}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("rotate refusal status = %d, want 422", code)
	}
	for _, want := range []string{
		`value="nope"`, "That&#39;s not a duration — like 720h or 30d.",
		`data-modal="secret-rotate-ssh-key"`,
		`id="se-exp-ssh-key-meta"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rotate 422 missing %q", want)
		}
	}
	for _, forbidden := range []string{"rotatedsecretvalue", tok} {
		if strings.Contains(page, forbidden) {
			t.Errorf("rotate 422 re-rendered a secret %q", forbidden)
		}
	}
}

// TestSecretsUsedByLegColumn is the GATE for A2's "who uses this": the
// leg-owned sealed secrets (ntfy/*, deadman/heartbeat_url) list their /system
// leg with a link in the Used by cell, so a rotation names its consumer. A
// secret no leg reads stays leg-free. Dropping the leg line must red.
func TestSecretsUsedByLegColumn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, sec := range []store.SecretRow{
		{Name: "ntfy/token", Kind: "token", Source: store.SecretSourceUser, Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "ntfy/token|token"},
		{Name: "deadman/heartbeat_url", Kind: "url", Source: store.SecretSourceUser, Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "deadman/heartbeat_url|url"},
		{Name: "api-token", Kind: "token", Source: store.SecretSourceUser, Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "api-token|token"},
	} {
		if _, err := h.store.CreateSecret(ctx, sec); err != nil {
			t.Fatalf("CreateSecret %s: %v", sec.Name, err)
		}
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/secrets")
	if code != http.StatusOK {
		t.Fatalf("/secrets = %d, want 200", code)
	}
	for _, want := range []string{
		`href="/system/ntfy"`,
		`href="/system/deadman"`,
		"ntfy/token",
		"deadman/heartbeat_url",
		"api-token",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/secrets used-by column missing %q", want)
		}
	}
	// The generic secret is leg-free: its row's used-by cell shows no leg.
	if strings.Contains(page, `id="secret-api-token"`) &&
		!strings.Contains(page, "unbound — safe to remove") {
		t.Error("generic secret lost its unbound note")
	}
}

// TestSecretsKeyDeepLinkFocus is the GATE for A2's deep-link focus: /secrets?
// key=ntfy/token (the href m75's Seal-token verb renders) focuses that row;
// an unknown key leaves every row unfocused. Dropping the focus must red.
func TestSecretsKeyDeepLinkFocus(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "ntfy/token", Kind: "token", Source: store.SecretSourceUser,
		Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "ntfy/token|token",
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/secrets?key=ntfy/token")
	if code != http.StatusOK {
		t.Fatalf("/secrets?key= = %d, want 200", code)
	}
	if !strings.Contains(page, `id="secret-ntfy/token" class="row-focus"`) {
		t.Error("/secrets?key=ntfy/token does not focus the row")
	}

	code, page = getBody(t, client, ts.URL+"/secrets?key=nope")
	if code != http.StatusOK {
		t.Fatalf("/secrets?key=nope = %d, want 200", code)
	}
	if strings.Contains(page, `class="row-focus"`) {
		t.Error("/secrets?key=nope focused a row")
	}
}

// TestSecretsInlineColumnsAndDetail is the GATE for the secrets list redesign:
// the inline row is the scan set only (Name · Kind · Source · Expiry · Last
// used, + Actions), the row opens a native details dialog (data-modal-row),
// the name cell carries the keyboard-reachable opener, and the dialog carries
// the facts that left the row (Created, Last rotated, Used by) plus the row's
// actions. The sealed value never renders in either surface.
func TestSecretsInlineColumnsAndDetail(t *testing.T) {
	h := newHarness(t)
	seedSecrets(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/secrets")
	if code != http.StatusOK {
		t.Fatalf("/secrets = %d, want 200", code)
	}

	// The inline header is exactly the scan set; the moved facts are no longer
	// columns.
	for _, want := range []string{
		`<th scope="col">Name</th>`,
		`<th scope="col">Kind</th>`,
		`<th scope="col">Source</th>`,
		`<th scope="col">Expiry</th>`,
		`<th scope="col">Last used</th>`,
		`<th scope="col" class="right">Actions</th>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/secrets inline table missing column %q", want)
		}
	}
	for _, gone := range []string{
		`<th scope="col">Created</th>`,
		`<th scope="col">Last rotated</th>`,
		`<th scope="col">Used by</th>`,
	} {
		if strings.Contains(page, gone) {
			t.Errorf("/secrets still renders the removed column %q", gone)
		}
	}

	// The row-open hook: a pointer row (data-modal-row) plus a keyboard opener
	// in the name cell that opens the same dialog. The actions cell carries the
	// sec-actions scope the chevron-order rule hangs off.
	for _, want := range []string{
		`data-modal-row="secret-detail-api-token"`,
		`<button type="button" class="cellact sec-name" data-modal-open="secret-detail-api-token">api-token</button>`,
		`<td class="sec-actions" data-label="Actions">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/secrets missing the row-open hook %q", want)
		}
	}

	// The details dialog carries the full record, the moved facts, and the
	// row's actions.
	for _, want := range []string{
		`id="secret-detail-api-token"`,
		`aria-labelledby="secret-detail-api-token-title"`,
		"<dt>Kind</dt>", "<dt>Source</dt>", "<dt>Created</dt>",
		"<dt>Last rotated</dt>", "<dt>Last used</dt>", "<dt>Expiry</dt>",
		"<dt>Used by</dt>",
		`used by <span class="mono">deploy-app</span> as <span class="mono">$API_TOKEN</span>`,
		`aria-label="Rotate"`, `aria-label="Remove"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/secrets detail dialog missing %q", want)
		}
	}

	// Invariant 4: neither the row nor the detail dialog renders the value.
	for _, forbidden := range []string{"CIPHER-SENTINEL", "NONCE-SENTINEL"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("/secrets leaked sealed material %q", forbidden)
		}
	}
}

// TestSecretsRowLayoutCSS pins the R2 layout fixes: the desktop secret name is
// left-aligned (only the ≤719px stacked card right-aligns it against its label),
// the conditional Bindings chevron is prepended to the LEFT of the constant
// Rotate/Remove glyphs so their x never changes between rows, and the Actions
// header right-aligns over the glyph cluster.
func TestSecretsRowLayoutCSS(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		// The desktop name rule carries no text-align, so a wrapped name reads
		// left like every other cell.
		".sec-name { overflow-wrap: anywhere; white-space: normal; }",
		// The conditional chevron holds the left slot; the constant verbs stay
		// pinned to the right edge.
		".sec-actions .rowacts .ramenu { order: -1; }",
		// Column proportions keep the six scan columns even.
		".sec-table th:nth-child(5) { width: 20%; }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing secrets layout rule %q", want)
		}
	}
	// The right-align of the name is scoped to the phone block: the rule must
	// appear after the one 719px breakpoint, never in the desktop cascade.
	phone := strings.Index(app, "@media (max-width: 719px)")
	phoneName := strings.Index(app, ".tablewrap.stackable td .sec-name { min-width: 0; text-align: right; }")
	if phone < 0 || phoneName < 0 || phoneName < phone {
		t.Errorf("the .sec-name right-align must live inside the ≤719px block (phone=%d rule=%d)", phone, phoneName)
	}
}
