package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// seedRunners registers a cached and an on-the-fly runner, exercising both
// build modes and both cap fields. The bootstrap body carries a sentinel so a
// test can prove it never reaches the page — the wire projection has no field
// for it, and neither does this surface.
func seedRunners(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()

	if _, err := h.store.CreateRunner(ctx, store.Runner{
		Name:        "fedora-ee",
		Mode:        store.RunnerModeCached,
		Base:        "registry.fedoraproject.org/fedora-minimal:44",
		Bootstrap:   "microdnf install -y sshpass rsync\nBOOTSTRAP-BODY-SENTINEL\n",
		Packages:    []string{"rsync", "sshpass"},
		AnsibleCore: "2.18.1",
		MemoryMax:   "2G",
		CPUQuota:    "200%",
	}); err != nil {
		t.Fatalf("seed fedora-ee: %v", err)
	}
	if _, err := h.store.CreateRunner(ctx, store.Runner{
		Name:          "dev-shell",
		Mode:          store.RunnerModeOnTheFly,
		Base:          "registry.fedoraproject.org/fedora-minimal:44",
		Containerfile: ".exocomp/Containerfile",
	}); err != nil {
		t.Fatalf("seed dev-shell: %v", err)
	}
}

// TestRunnersSurfaceGated is the gate for the runners view: token-gated
// (no session refused, session renders), shows every column, carries NO
// bootstrap body anywhere on the page, and the nav carries Runners.
func TestRunnersSurfaceGated(t *testing.T) {
	h := newHarness(t)
	seedRunners(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// No session: refused with a redirect to the login form, never a render.
	noSession := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noSession.Get(ts.URL + "/runners")
	if err != nil {
		t.Fatalf("GET /runners no session: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/runners without session = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("/runners redirect = %q, want /login…", loc)
	}

	// With a session: the surface renders.
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, client, ts.URL+"/runners")
	if code != http.StatusOK {
		t.Fatalf("/runners with session = %d, want 200", code)
	}

	// Columns and rows.
	for _, want := range []string{
		`scope="col"`, "All runners",
		"fedora-ee", "dev-shell",
		`<th scope="col">Name</th>`,
		`<th scope="col">Mode</th>`,
		`<th scope="col">Base</th>`,
		`<th scope="col">Packages</th>`,
		`<th scope="col">ansible-core</th>`,
		`<th scope="col">Caps</th>`,
		`<th scope="col">Hash</th>`,
		`<th scope="col">Created</th>`,
		"registry.fedoraproject.org/fedora-minimal:44",
		"rsync, sshpass", // short package list, joined
		"2.18.1",
		"mem 2G", "cpu 200%",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/runners missing field %q", want)
		}
	}

	// Mode chips, both states, always labelled.
	for _, want := range []string{
		`<span class="chip ok"><span class="dot"></span>CACHED</span>`,
		`<span class="chip wait"><span class="dot"></span>ONTHEFLY</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/runners missing mode chip %q", want)
		}
	}

	// Operator write surface: an add form and per-row remove. Runner
	// definitions hold no secret material, so this lane is operator-scope
	// only — no confirm_token, unlike secrets/tokens.
	for _, want := range []string{
		`action="/runners/add"`,
		`action="/runners/fedora-ee/rm"`,
		`action="/runners/dev-shell/rm"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/runners (operator) missing write control %q", want)
		}
	}
	if strings.Contains(page, "confirm_token") {
		t.Error("/runners must not be reauth-gated; it holds no secret material")
	}

	// A readonly session gets no mutation control, only the CLI hint.
	roClient := login(t, ts.URL, h.mint(t, "viewer", "readonly"))
	roCode, roPage := getBody(t, roClient, ts.URL+"/runners")
	if roCode != http.StatusOK {
		t.Fatalf("/runners readonly = %d, want 200", roCode)
	}
	if strings.Contains(roPage, "<form") || strings.Contains(roPage, `method="post"`) {
		t.Error("/runners (readonly) must expose no mutation form")
	}
	if !strings.Contains(roPage, "needs an operator token") {
		t.Error("/runners (readonly) missing the read-only hint")
	}

	// The nav carries Runners, marked current on this page.
	if !strings.Contains(page, `href="/runners" aria-current="page"`) {
		t.Error("/runners nav item is not present/current")
	}

	// NO bootstrap body anywhere: neither the raw script nor its sentinel line
	// may reach the page. Only a digest/line-count would ever be permissible,
	// and this surface shows neither.
	if strings.Contains(page, "BOOTSTRAP-BODY-SENTINEL") {
		t.Error("/runners leaked the bootstrap script body")
	}
	if strings.Contains(page, "microdnf install") {
		t.Error("/runners leaked the bootstrap script body")
	}
}

// TestRunnersNavElsewhere: the Runners nav item appears on other pages too, so
// it is reachable from anywhere in the surface.
func TestRunnersNavElsewhere(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/")
	if !strings.Contains(page, `href="/runners"`) || !strings.Contains(page, ">Runners</span>") {
		t.Error("dashboard nav missing the Runners link")
	}
}
