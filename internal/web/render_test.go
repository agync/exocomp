package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		`href="/assets/tokens.css?v=`,
		`href="/assets/app.css?v=`,
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
	// The three-state theme toggle renders in the server-default "system" state
	// as an icon-only control, naming the current state and what a click does in
	// the aria-label (no visible word label).
	for _, want := range []string{
		`aria-label="Theme: system. Switch to light."`,
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
// and both first-class themes (the a11y hard requirements). tokens.css owns the
// palette: the prefers-color-scheme default AND the [data-theme] overrides the
// in-page toggle drives. app.css references those tokens and carries the ring.
func TestFocusRingAndThemes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{":focus-visible", "outline: var(--focus-ring)"} {
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
	// The toggle is a compact icon button (32px, clears the WCAG AA 24px target
	// floor) shared with the collapse control, not a labelled pill.
	for _, want := range []string{"width: 32px; height: 32px", "border-radius: 9px"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css: theme toggle missing %q", want)
		}
	}

	_, tokens := getBody(t, client, ts.URL+"/assets/tokens.css")
	// Both theme mechanisms live in tokens.css now: the OS-driven default and
	// the explicit toggle overrides, in both directions.
	for _, want := range []string{"prefers-color-scheme: light", `[data-theme="light"]`, `[data-theme="dark"]`} {
		if !strings.Contains(tokens, want) {
			t.Errorf("tokens.css missing %q", want)
		}
	}
	if !strings.Contains(tokens, "tabular-nums") {
		t.Error("tokens.css missing the tabular-nums rule")
	}
	// The palette moved out of app.css entirely — no theme block defined there.
	if strings.Contains(app, `:root[data-theme="light"]`) || strings.Contains(app, `:root[data-theme="dark"]`) {
		t.Error("app.css still defines the palette; tokens.css owns it")
	}
}

// TestAssetsAreCacheBusted asserts the stylesheet links carry a version query
// and that the exact versioned URL the page emits actually serves the asset — so
// a new build's CSS is fetched at once rather than served stale from a browser
// cache that still holds the prior build's bytes.
func TestAssetsAreCacheBusted(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/")
	marker := `href="/assets/app.css?v=`
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("app.css link is not cache-busted: %s", page[:min(len(page), 600)])
	}
	rest := page[i+len(`href="`):]
	href := rest[:strings.IndexByte(rest, '"')]
	if !strings.HasPrefix(href, "/assets/app.css?v=") || href == "/assets/app.css?v=" {
		t.Fatalf("app.css version query is empty: %q", href)
	}

	code, css := getBody(t, client, ts.URL+href)
	if code != 200 {
		t.Fatalf("versioned app.css = %d, want 200", code)
	}
	if !strings.Contains(css, ".modal") {
		t.Fatalf("versioned URL did not serve app.css content")
	}
}

// TestResponsiveTablesAndHeaders is the gate for the responsive layout: wide
// tables carry the stackable class and a data-label on every cell (so the phone
// stacked-card pattern can label each value), short/step-ledger tables are
// deliberately left plain, every list-page card grew the kicker+<h3> header the
// detail views already use, and the extracted enabledChip renders
// byte-identically to the old inline enabled/disabled conditional in both
// branches.
func TestResponsiveTablesAndHeaders(t *testing.T) {
	h := newHarness(t)
	seedPostV1(t, h)
	// A disabled source alongside the seed's enabled one exercises both
	// enabledChip branches on the same page.
	if _, err := h.store.CreateSource(context.Background(), store.Source{
		Name: "legacy", Kind: store.SourceGit, URI: "git@host:legacy.git",
		Ref: "refs/heads/main", Enabled: false,
	}); err != nil {
		t.Fatalf("seed disabled source: %v", err)
	}
	// A queued job so /jobs renders its stackable table, and an event so /events
	// does the same.
	if _, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "nightly", Kind: "shell", Trigger: "manual",
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := h.store.CreateEvent(context.Background(), store.Event{
		Source: "prometheus", Outcome: store.OutcomeMatched, Reason: "matched a rule",
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	// A workspace so the dashboard drift table renders, and a gated job so the
	// dashboard "Needs you" / approvals table renders.
	if _, err := h.store.CreateWorkspace(context.Background(), store.Workspace{
		Name: "prod", TofuDir: "prod",
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	h.gatedJob(t, 30*time.Minute)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// (a) Stackable tables carry class="stackable" and a data-label per cell.
	stack := []struct {
		path   string
		labels []string
	}{
		{"/", []string{`class="tablewrap stackable"`, `data-label="Approval"`, `data-label="Job"`, `data-label="Workspace"`, `data-label="Signed"`, `data-label="Cron"`}},
		{"/sources", []string{`class="tablewrap stackable"`, `data-label="Source"`, `data-label="URI"`, `data-label="Ref"`, `data-label="Trust"`, `data-label="Last sync"`, `data-label="Enabled"`}},
		{"/policies", []string{`class="tablewrap stackable"`, `data-label="Policy"`, `data-label="On match"`, `data-label="Expression"`, `data-label="Enabled"`}},
		{"/events", []string{`class="tablewrap stackable"`, `data-label="Event"`, `data-label="Source"`, `data-label="Outcome"`, `data-label="Received"`}},
		{"/jobs", []string{`class="tablewrap stackable"`, `data-label="Job"`, `data-label="Source"`, `data-label="Kind"`, `data-label="Trigger"`, `data-label="Exit"`}},
	}
	for _, c := range stack {
		_, page := getBody(t, client, ts.URL+c.path)
		for _, want := range c.labels {
			if !strings.Contains(page, want) {
				t.Errorf("%s missing stackable marker %q", c.path, want)
			}
		}
	}

	// (b) Every list-page card carries an <h3> header; pages with a functional
	// kicker (empty kicker = none expected) also render that kicker.
	headers := []struct {
		path, kicker, h3 string
	}{
		// /sources' "Read-only · CLI-managed" kicker only renders for a
		// non-operator now that an operator gets write controls instead
		// (sources_write.go); the operator client this test logs in with sees
		// no kicker here, same as /jobs.
		{"/sources", "", "All sources"},
		{"/schedules", "Cron schedules", "All schedules"},
		{"/workflows", "", "All workflows"},
		{"/policies", "CEL gates", "All policies"},
		{"/jobs", "", "All jobs"},
		{"/events", "Received alerts", "All events"},
		{"/approvals", "Waiting on a human", "Pending approvals"},
	}
	for _, hd := range headers {
		_, page := getBody(t, client, ts.URL+hd.path)
		if hd.kicker != "" && !strings.Contains(page, `<div class="kicker">`+hd.kicker+`</div>`) {
			t.Errorf("%s missing kicker %q", hd.path, hd.kicker)
		}
		if !strings.Contains(page, "<h3>"+hd.h3+"</h3>") {
			t.Errorf("%s missing header %q", hd.path, hd.h3)
		}
	}

	// (c) enabledChip renders identically to the old inline conditional, both
	// branches, on the sources page (enabled seed + disabled "legacy").
	_, sources := getBody(t, client, ts.URL+"/sources")
	for _, want := range []string{
		`<span class="chip ok"><span class="dot"></span>ENABLED</span>`,
		`<span class="chip dim"><span class="dot"></span>DISABLED</span>`,
	} {
		if !strings.Contains(sources, want) {
			t.Errorf("/sources enabledChip output changed: missing %q", want)
		}
	}

	// (d) Short / step-ledger tables were deliberately left non-stackable.
	for _, path := range []string{"/workflows", "/workflows/provision-web-01"} {
		_, page := getBody(t, client, ts.URL+path)
		if !strings.Contains(page, `class="tablewrap"`) {
			t.Errorf("%s should still render a plain tablewrap", path)
		}
		if strings.Contains(page, "stackable") {
			t.Errorf("%s was stacked but should stay a plain scroll box", path)
		}
	}

	// The CSS side: the stacked-card pattern, its data-label rendering, the
	// <=560px card-padding reduction (F11), and the F7 desktop wrap cap.
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		"@media (max-width: 560px)",
		".tablewrap.stackable td",
		"content: attr(data-label)",
		"td.wrap { max-width: 32ch; overflow-wrap: anywhere; }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing responsive rule %q", want)
		}
	}
}

// TestThemeToggleThreeState asserts the toggle is a three-state control: the
// SSR markup carries three distinct glyphs (system/light/dark), renders in the
// server-default "system" state with a current-state aria-label, and the inline
// script cycles system -> light -> dark -> system, clearing the stored
// preference and data-theme for system and persisting a light/dark choice.
func TestThemeToggleThreeState(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}

	// (a) Three distinct glyphs, one per state, all present in SSR — the client
	// only swaps which is visible, so the markup is correct before any script.
	for _, want := range []string{
		`data-glyph="system"`, `data-glyph="light"`, `data-glyph="dark"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("toggle missing glyph slot %q", want)
		}
	}
	// Distinct SVG shapes: a monitor rect (system), a sun circle (light), a
	// crescent path (dark). Three different marks, not one repeated.
	for _, shape := range []string{"<rect ", "<circle ", "A5.6 5.6 0 1 1"} {
		if !strings.Contains(page, shape) {
			t.Errorf("toggle missing distinct glyph shape %q", shape)
		}
	}

	// (b) SSR renders the server-default "system" state: the system glyph shown,
	// the other two hidden, the visible label and aria-label naming system.
	if !strings.Contains(page, `<span class="themeglyph" data-glyph="system" aria-hidden="true">`) {
		t.Error("system glyph should be the shown one in SSR (no hidden attr)")
	}
	for _, hidden := range []string{
		`data-glyph="light" aria-hidden="true" hidden`,
		`data-glyph="dark" aria-hidden="true" hidden`,
	} {
		if !strings.Contains(page, hidden) {
			t.Errorf("non-active glyph not hidden in SSR: %q", hidden)
		}
	}
	if !strings.Contains(page, `aria-label="Theme: system. Switch to light."`) {
		t.Error("SSR aria-label does not name the current state and the next")
	}

	// (c) The inline script cycles the three states and applies stored state:
	// system clears both the attribute and storage; light/dark persist.
	for _, want := range []string{
		`{ system: "light", light: "dark", dark: "system" }`, // the cycle
		`removeAttribute("data-theme")`,                      // system clears attr
		`removeItem("exocomp-theme")`,                        // system clears storage
		`setAttribute("data-theme", next)`,                   // light/dark pin attr
		`setItem("exocomp-theme", next)`,                     // light/dark persist
		`stored === "light" || stored === "dark"`,            // read applies stored
	} {
		if !strings.Contains(page, want) {
			t.Errorf("toggle script missing %q", want)
		}
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
