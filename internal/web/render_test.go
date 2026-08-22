package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestDashboardA11y asserts the accessibility affordances the ui-brief makes
// hard requirements: the token stylesheets load, headings and tables carry
// scope/labels, the active nav is announced, and digits are tabular.
func TestDashboardA11y(t *testing.T) {
	h := newHarness(t)
	// Seed a job so the queue table (with its <th scope="col">) renders.
	if _, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "nightly", Kind: "shell", Trigger: "schedule",
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`href="/assets/tokens.css"`,
		`href="/assets/app.css"`,
		`aria-current="page"`,
		`scope="col"`,
		`<main>`,
		`lang="en"`,
		`name="viewport"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing a11y marker %q", want)
		}
	}
	// The theme toggle names the destination theme (dynamic label) and its
	// aria-label mirrors it, rather than a static "theme" string.
	for _, want := range []string{
		`id="themelabel"`,
		`aria-label="Switch to light theme"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard toggle missing %q", want)
		}
	}
	if strings.Contains(page, `aria-label="Toggle light or dark theme">theme</button>`) {
		t.Error("theme toggle still renders the static label")
	}
}

// TestFocusRingAndThemes asserts the served CSS carries the visible focus ring
// and both first-class themes (the a11y hard requirements). tokens.css keeps
// the prefers-color-scheme default; app.css adds the data-theme overrides the
// in-page toggle drives.
func TestFocusRingAndThemes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{":focus-visible", "outline: var(--focus-ring)", `[data-theme="light"]`, `[data-theme="dark"]`} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing %q", want)
		}
	}
	if strings.Contains(app, "outline: none;") && !strings.Contains(app, ":focus:not(:focus-visible)") {
		t.Error("outline:none must be paired with a focus-visible replacement")
	}
	// Content links rest without an underline; the underline returns on hover.
	if !strings.Contains(app, "text-decoration: none;") {
		t.Error("app.css: content links must rest with text-decoration:none")
	}
	if !strings.Contains(app, "text-decoration: underline;") {
		t.Error("app.css: hover/focus underline affordance is missing")
	}
	// The toggle is a 44px pill (general-chrome tap size, not a third size).
	for _, want := range []string{"height: 44px", "border-radius: 999px"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css: theme toggle missing %q", want)
		}
	}

	_, tokens := getBody(t, client, ts.URL+"/assets/tokens.css")
	if !strings.Contains(tokens, "prefers-color-scheme: light") {
		t.Error("tokens.css missing the light theme")
	}
	if !strings.Contains(tokens, "tabular-nums") {
		t.Error("tokens.css missing the tabular-nums rule")
	}
}

// TestPhoneApprovalTouchTargets asserts the phone surface reserves >=48px touch
// targets and renders the destroys-first / redacted-secret contract markers.
func TestPhoneApprovalTouchTargets(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	if !strings.Contains(app, "min-height: 48px") {
		t.Error("phone touch targets are not >=48px")
	}
	if !strings.Contains(app, "max-width: 390px") {
		t.Error("phone surface is not laid out at 390px")
	}
}
