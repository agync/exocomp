package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestNetgroupFormAtRest renders the Netgroups tab for an operator and asserts
// the forms-round anatomy: the add dialog carries the reusable field pattern
// with always-present meta lines (help text at rest), the aria wiring tying
// each control to its meta, a form-status line, and novalidate so the daemon
// stays authoritative.
func TestNetgroupFormAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/checks?tab=netgroups")
	if code != http.StatusOK {
		t.Fatalf("/checks?tab=netgroups = %d, want 200", code)
	}
	for _, want := range []string{
		`id="netgroups-add"`,
		`action="/netgroups/add"`,
		`id="netgroup-add-form" novalidate`,
		`aria-describedby="ng-name-meta"`,
		`aria-describedby="ng-cidr-meta"`,
		`id="ng-name-meta"`,
		`id="ng-cidr-meta"`,
		"Letters, digits, dots, hyphens, underscores.",
		"Comma-separated. IPv4 or IPv6.",
		`role="status" aria-live="polite"`,
		"Add netgroup",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("netgroup form missing %q", want)
		}
	}
	// At rest the control carries no aria-invalid (the next attribute is
	// aria-describedby) and the meta line is plain, not .is-error.
	if !strings.Contains(page, `placeholder="home-cidr" aria-describedby="ng-name-meta">`) {
		t.Error("at-rest name control carries an aria-invalid refusal")
	}
	if !strings.Contains(page, `id="ng-name-meta" class="meta">Letters`) {
		t.Error("at-rest name meta is not plain help text")
	}
}

// postNetgroup sends a netgroup-add POST and returns status + body without
// following a redirect, for the 422-render assertions.
func postNetgroup(t *testing.T, client *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	resp := postForm(t, client, target, form, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body)
}

// TestNetgroupFormRefusalsRender asserts the 422 re-render fills both meta
// lines and the fail rings at once, names the first invalid control for focus,
// and emits the JS that opens the dialog for the JS-off path.
func TestNetgroupFormRefusalsRender(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := postNetgroup(t, op, ts.URL+"/netgroups/add",
		url.Values{"name": {"Home CIDR"}, "cidrs": {"192.168.0/16"}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("refusal status = %d, want 422", code)
	}
	for _, want := range []string{
		`aria-invalid="true"`, "Names can&#39;t have spaces.", "Use home-cidr",
		"192.168.0/16", "needs four octets",
		`exoModalOpen("netgroups-add")`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("422 re-render missing %q", want)
		}
	}
	if !strings.Contains(page, `id="ng-name"`) {
		t.Error("422 re-render missing the name control")
	}
}

// TestNetgroupFormDuplicateRenders asserts a duplicate refusal lands on the
// name field's meta line and keeps the CIDRs meta at rest.
func TestNetgroupFormDuplicateRenders(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}

	code, page := postNetgroup(t, op, ts.URL+"/netgroups/add",
		url.Values{"name": {"home"}, "cidrs": {"192.168.0.0/16"}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate status = %d, want 422", code)
	}
	if !strings.Contains(page, "A netgroup called home already exists.") {
		t.Errorf("duplicate 422 missing the name-field sentence")
	}
	// The CIDR field is valid and at rest: it must not be flagged invalid. The
	// "needs four octets" string now ships in the on-blur validation JS on
	// every page, so assert the field's state (aria-invalid), not the copy.
	if strings.Contains(page, `id="ng-cidr"`+"\" aria-invalid") || strings.Contains(page, `id="ng-cidr" aria-invalid`) {
		t.Error("duplicate 422 flagged the cidr field invalid")
	}
}
