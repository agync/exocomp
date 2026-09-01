package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// seedDestructiveObjects registers one of each object whose destructive
// action (Remove/Delete/Revoke) must confirm before it fires, so every
// operator-visible destructive form renders.
func seedDestructiveObjects(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()

	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	}); err != nil {
		t.Fatalf("seed check: %v", err)
	}
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}
	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly", CronExpr: "0 3 * * *", Kind: "backup",
	}); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	if _, err := h.store.CreateSource(ctx, store.Source{
		Name: "infra", Kind: store.SourceGit, URI: "git@host:infra.git",
		Ref: "refs/heads/main", Enabled: true,
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	if _, err := h.store.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-destroy", Workspace: "prod",
		Expr:   "plan.resource_changes.exists(c, c.change.actions == ['delete'])",
		Action: store.PolicyRequireApproval,
	}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	if _, err := h.store.CreateRunner(ctx, store.Runner{
		Name: "fedora-ee", Mode: store.RunnerModeCached,
		Base: "registry.fedoraproject.org/fedora-minimal:44",
	}); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	// The secrets Delete form sits outside the SealAvailable block, so a
	// direct store seed renders it without an envelope.
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "ssh-key", Kind: "generic", Source: store.SecretSourceUser,
		Nonce: []byte("NONCE-SENTINEL-D"), Ciphertext: []byte("CIPHER-SENTINEL-D"),
		AAD: "ssh-key|generic",
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	h.mint(t, "seed-token", "readonly")
}

// TestConfirmDialogInLayout is the gate for the shared confirm modal: every
// layout carries the native <dialog>, its message slot, the Cancel/Confirm
// buttons, and the form[data-confirm] submit hook that opens it and
// re-submits the waiting form via requestSubmit — never window.confirm.
func TestConfirmDialogInLayout(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != http.StatusOK {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<dialog class="modal" id="confirm-dialog"`,
		`id="confirm-title">Confirm destructive action</h2>`,
		`id="confirm-msg"`,
		`id="confirm-ok"`,
		`data-modal-dismiss`, // the × and Cancel button both dismiss the dialog
		`form[data-confirm]`, // the submit hook targets the marked forms
		`f.requestSubmit()`,  // Confirm re-submits the waiting form
	} {
		if !strings.Contains(page, want) {
			t.Errorf("layout missing confirm-dialog marker %q", want)
		}
	}
}

// TestDestructiveFormsCarryDataConfirm is the gate for the destructive-action
// guard: every Remove/Delete/Revoke form carries data-confirm="<action>?" (the
// message the shared confirm dialog shows) alongside the hidden confirmed=1
// field the write handlers already require. Operator session only — the forms
// render for an operator alone.
func TestDestructiveFormsCarryDataConfirm(t *testing.T) {
	h := newHarness(t)
	seedDestructiveObjects(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	cases := []struct {
		path string
		want string
	}{
		{"/checks", `data-confirm="Remove check at-home?"`},
		{"/netgroups", `data-confirm="Remove netgroup home?"`},
		{"/schedules", `data-confirm="Remove schedule nightly?"`},
		{"/sources", `data-confirm="Delete source infra?"`},
		{"/policies", `data-confirm="Remove policy no-destroy?"`},
		{"/tokens", `data-confirm="Revoke token seed-token?"`},
		{"/runners", `data-confirm="Remove runner fedora-ee?"`},
		{"/secrets", `data-confirm="Delete secret ssh-key?"`},
	}
	for _, c := range cases {
		code, page := getBody(t, op, ts.URL+c.path)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", c.path, code)
		}
		if !strings.Contains(page, c.want) {
			t.Errorf("%s missing %q", c.path, c.want)
		}
		// The hidden confirmed=1 field stays alongside the UI guard — the
		// confirm dialog is the UI layer, confirmed=1 the server-side signal.
		if !strings.Contains(page, `name="confirmed" value="1"`) {
			t.Errorf("%s lost the hidden confirmed=1 field", c.path)
		}
	}

	// A readonly session sees no destructive form, so no data-confirm marker.
	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))
	for _, c := range cases {
		code, page := getBody(t, ro, ts.URL+c.path)
		if code != http.StatusOK {
			t.Fatalf("%s (readonly) status = %d, want 200", c.path, code)
		}
		if strings.Contains(page, "data-confirm=") {
			t.Errorf("%s (readonly) leaked a confirm-marked destructive form", c.path)
		}
	}
}

// TestConfirmDialogRendersOnEverySurface: the shared confirm dialog ships on
// every authenticated surface, not just the dashboard — a destructive action
// is reachable from the page that owns it.
func TestConfirmDialogRendersOnEverySurface(t *testing.T) {
	h := newHarness(t)
	seedDestructiveObjects(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, path := range []string{"/", "/checks", "/netgroups", "/schedules", "/sources", "/policies", "/tokens", "/runners", "/secrets"} {
		code, page := getBody(t, op, ts.URL+path)
		if code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, code)
		}
		if !strings.Contains(page, `id="confirm-dialog"`) {
			t.Errorf("%s missing the shared confirm dialog", path)
		}
	}
}
