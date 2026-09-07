package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/netip"
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
	// A long-running queued job so the dashboard's queue card renders a row
	// (a bare job with no argv would settle before the GET and drop off the
	// active queue), and a schedule so the Schedules card renders its count
	// card (the dashboard carries no tables at all after the summary-card
	// redesign — each card is a labeled link with its own aria-labelledby
	// heading, the queue card a set of row links).
	if _, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "nightly", Kind: "shell", Trigger: "schedule",
		Args: store.JobArgs{Argv: []string{"/bin/sleep", "30"}},
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := h.store.CreateSchedule(context.Background(), store.NewSchedule{
		Name: "nightly-backup", CronExpr: "0 3 * * *", Kind: "backup",
	}); err != nil {
		t.Fatalf("seed schedule: %v", err)
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
		`class="jobs-row queue-row"`,
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
	// A workspace so the dashboard's workspace card has a nonzero total, and a
	// gated job so /approvals renders its stackable table.
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
		// "/" is excluded here: the dashboard redesign replaced its embedded
		// tables with summary cards (view_dashboard_test.go covers those).
		// The Schedules card followed in F7, so no table survives on "/".
		{"/sources", []string{`class="tablewrap stackable"`, `data-label="Source"`, `data-label="URI"`, `data-label="Ref"`, `data-label="Trust"`, `data-label="Last sync"`, `data-label="Enabled"`}},
		{"/policies", []string{`class="tablewrap stackable"`, `data-label="Policy"`, `data-label="On match"`, `data-label="Expression"`, `data-label="Enabled"`}},
		// /events is deliberately absent: its tray table is a grid of row
		// buttons with an inline expand pane, never a stackable table.
		// /jobs is deliberately absent: its two-line rows (2a) are a grid of
		// links, not a stackable table.
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
		// /events is deliberately absent: the rebuilt page leads with the
		// funnel card and the tray table, not a kicker + All-events heading.
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

	// (c) enabledChip on the sources column hides the default ENABLED (the
	// column header carries the default, F12) and keeps the divergent DISABLED
	// (enabled seed renders nothing; disabled "legacy" still chips).
	_, sources := getBody(t, client, ts.URL+"/sources")
	if !strings.Contains(sources, `<span class="chip dim"><span class="dot"></span>DISABLED</span>`) {
		t.Errorf("/sources enabledChip: DISABLED chip missing")
	}
	if strings.Contains(sources, `<span class="chip ok"><span class="dot"></span>ENABLED</span>`) {
		t.Errorf("/sources enabledChip: default ENABLED chip should be hidden (F12)")
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

// TestApproveBlockMT26Separation pins the deliberate 26px top separation of
// the phone approval surface's global-kill details block: the inline style it
// replaced became a single-class .mt26, which a later .approve-block rule
// would collapse to its own 14px margin — the compound selector restores
// the 26px, and the surface still renders the class combo it must apply to.
func TestApproveBlockMT26Separation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	if !strings.Contains(app, ".approve-block.mt26 { margin-top:  26px; }") {
		t.Error("app.css missing the approve-block.mt26 compound rule")
	}

	_, approval := h.gatedJob(t, time.Minute)
	code, page := getBody(t, client, ts.URL+fmt.Sprintf("/approvals/%d", approval.ID))
	if code != 200 {
		t.Fatalf("approval surface status = %d, want 200", code)
	}
	if !strings.Contains(page, `<details class="approve-block mt26">`) {
		t.Error("approval surface lost the approve-block mt26 class combo")
	}
}

// TestAppbarAndNavGroups is the GATE for F15/F7: the shell owns the page title
// in a slim app bar (with the page's primary verb at the right), the sidebar is
// grouped by verb, and actionable-state badges show on Checks (fail) and
// Approvals (wait) — never a pill, always a count.
func TestAppbarAndNavGroups(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// A down probe target drives the Checks fail badge; a pending approval the
	// Approvals wait badge.
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name: "home", Prefixes: []netip.Prefix{mustPrefix(t, "192.168.1.0/24")},
	}); err != nil {
		t.Fatalf("seed netgroup: %v", err)
	}
	c, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("seed check: %v", err)
	}
	now := time.Now().UTC()
	if err := h.store.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: "laptop", Status: store.CheckDown,
		Consecutive: 3, Emitted: store.CheckDown,
		FirstSeen: now.Add(-time.Hour), LastChange: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("SetCheckState: %v", err)
	}
	_, _ = h.gatedJob(t, time.Minute)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<h1 class="appbar-title">Dashboard</h1>`,
		`class="navgroup">Operate</div>`,
		`class="navgroup">Automate</div>`,
		`class="navgroup">Provision</div>`,
		`class="navgroup">Access</div>`,
		`<span class="navbadge fail" aria-label="1 need attention">1</span>`,
		`<span class="navbadge wait" aria-label="1 need attention">1</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(page, `href="/netgroups"`) {
		t.Error("nav still carries a standalone Netgroups item (F7)")
	}

	// The sources list page's title and Add verb live in the app bar.
	code, src := getBody(t, client, ts.URL+"/sources")
	if code != 200 {
		t.Fatalf("/sources status = %d", code)
	}
	for _, want := range []string{
		`<h1 class="appbar-title">Sources</h1>`,
		`<span class="appbar-action">`,
		`>Add source</button>`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("/sources missing %q", want)
		}
	}
	if strings.Contains(src, `<h1>Sources</h1>`) {
		t.Error("page still renders its own display h1 alongside the app bar (F15)")
	}
}

// TestReviewCSSGuards pins the v0.4 CSS fixes that a markup test cannot see:
// the header primary verb is a 34px nowrap accent button (W1), and the
// state-tint rules (.fail-text/.wait-text) are declared AFTER .meta so a
// `meta mono fail-text` down-count actually renders fail, with .kv dd.wait-text
// beating the .kv dd colour (W12/W15). Reordering these breaks the guard.
func TestReviewCSSGuards(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	metaAt := strings.Index(app, ".meta { color: var(--ink-500); font: var(--type-body); }")
	failAt := strings.Index(app, ".fail-text { color: var(--signal-fail); }")
	waitAt := strings.Index(app, ".wait-text { color: var(--signal-wait); }")
	kvAt := strings.Index(app, ".kv dd.wait-text { color: var(--signal-wait); }")
	if failAt == -1 || waitAt == -1 || kvAt == -1 {
		t.Error("app.css missing the state-tint rules")
	}
	if failAt < metaAt || waitAt < metaAt {
		t.Error(".fail-text/.wait-text must be declared after .meta (W12/W15 specificity)")
	}
	if kvAt < waitAt {
		t.Error(".kv dd.wait-text must follow the bare .wait-text")
	}
	for _, want := range []string{
		".addbtn {",
		"min-height: 34px; height: 34px; padding: 0 14px",
		"white-space: nowrap",
		".appbar-meta { display: none; }",
		".powerline .pl-who { display: none; }",
		// UX-fix round: tray actions are a spaced flex row; the phone powerline
		// shows only the segments that need a decision (ok/dim hidden).
		".card.tray td[data-label=\"Actions\"] {",
		"display: flex; align-items: center; gap: 8px;",
		".pl-seg.ok, .pl-seg.dim { display: none; }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing rule %q", want)
		}
	}
}

// TestReviewHigh4CSSGuards pins the HIGH 4 CSS fixes that a markup test cannot
// see: ink-600 is for dots/rules/dividers, never text — the nav group labels,
// tray column headers, and log timestamps all read ink-500 (the last AA text
// step), while the crumb separator (a rule) stays ink-600 — and the font:
// shorthand on mono runs must not reset tabular-nums (re-asserted after the
// shorthand at both sites). Reordering or reverting either breaks the guard.
func TestReviewHigh4CSSGuards(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")

	// ink-600 is never text: the three text surfaces read ink-500, the crumb
	// separator (a rule, not text) keeps ink-600.
	for _, want := range []string{
		`color: var(--ink-500); padding: 0 10px 6px; margin-top: 16px;`, // nav group label
		`color: var(--ink-500); padding: 8px 16px 4px;`,                 // tray column header
		`.log .ts { color: var(--ink-500);`,                             // log timestamp
		`.crumb-sep { color: var(--ink-600); }`,                         // separator stays a rule
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing H4 ink fix %q", want)
		}
	}

	// tabular-nums must survive the font: shorthand on the shared mono block.
	mono := ruleDecl(app, ".mono, code, pre, .log {")
	fontAt := strings.Index(mono, "font: var(--type-log);")
	numsAt := strings.Index(mono, "font-variant-numeric: tabular-nums;")
	if fontAt == -1 || numsAt == -1 || numsAt < fontAt {
		t.Error(".mono, code, pre, .log must re-assert tabular-nums after the font: shorthand (H4)")
	}

	// The check-card header mono run repeats the same shorthand at a second site.
	cm := ruleDecl(app, ".checkcard > header .mono {")
	cf := strings.Index(cm, "font: var(--type-log);")
	cn := strings.Index(cm, "font-variant-numeric: tabular-nums;")
	if cf == -1 || cn == -1 || cn < cf {
		t.Error(".checkcard > header .mono must re-assert tabular-nums after the font: shorthand (H4)")
	}
}

// ruleDecl returns the declaration text of the first CSS rule beginning with
// prefix, up to its closing brace — so a guard can bound a property search to
// one rule instead of matching a later, unrelated occurrence in the sheet.
func ruleDecl(css, prefix string) string {
	i := strings.Index(css, prefix)
	if i < 0 {
		return ""
	}
	j := strings.Index(css[i:], "}")
	if j < 0 {
		return css[i:]
	}
	return css[i : i+j]
}

// TestReviewWebMediumCSSGuards pins the web MEDIUM CSS fixes: the check card's
// foot actions get the 30px row scale with ring-only danger (the `.btn.quiet`
// rule is scoped to `td` and would otherwise be inert there), share the phone's
// 48px touch floor, and the three narrow-viewport grids stack to one column
// instead of clipping a min-width track.
func TestReviewWebMediumCSSGuards(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.checkactions .btn.quiet {`,
		`.checkactions .btn.danger {`,
		`.btn { min-height: 48px; height: 48px; }`,
		`.cards, .attn-grid, .counts-grid, .targetgrid { grid-template-columns: 1fr; }`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing web-MEDIUM rule %q", want)
		}
	}
}

// TestW5EmptyStatesNameVerbAndTrigger is the GATE for W5: the empty states name
// the verb AND the trigger, per the review copy — not a bare "no X yet".
func TestW5EmptyStatesNameVerbAndTrigger(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, tc := range []struct{ path, want string }{
		{"/approvals", "Approvals land here when a rule or schedule needs your say."},
		{"/jobs", "They appear when a schedule, rule or manual run fires."},
		{"/events", "Point an alert source at exocomp ingest"},
	} {
		code, page := getBody(t, client, ts.URL+tc.path)
		if code != 200 {
			t.Fatalf("%s status = %d", tc.path, code)
		}
		if !strings.Contains(page, tc.want) {
			t.Errorf("%s empty state missing %q (W5)", tc.path, tc.want)
		}
	}
}

// TestReviewV05ACSSGuards pins the v0.5 lane-A CSS: the source-detail danger
// button is ring-only (R1), the writeform submit does not stretch full width
// (W11), and the check-card foot actions keep the row scale + phone floor (R2).
func TestReviewV05ACSSGuards(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.rowactions .btn.danger {`,
		`box-shadow: inset 0 0 0 1px var(--fail-border);`,
		`.writeform > .btn { align-self: flex-start; }`,
		`.checkactions .btn {`,
		`.checkactions .btn.danger {`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing v0.5-a rule %q", want)
		}
	}
}

// TestReviewV05BCSSGuards pins the v0.5 lane-B CSS: no tray zebra (R3), the
// 34px default button with the 44px .actions exception (R4), the blanket 48px
// phone floor, and the one-left-edge kicker alignment (R5).
func TestReviewV05BCSSGuards(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		".card.tray tbody tr:hover { background: var(--surface-row); }",
		"min-height: 34px; padding: 0 18px;",
		".actions .btn { width: 100%; min-height: var(--row-h); }",
		".phone .actions .btn { min-height: 48px; }",
		".card.tray .kicker, .card.tray > .meta { padding-left: 16px; }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing v0.5-b rule %q", want)
		}
	}
	// The tray zebra rule is gone entirely.
	if strings.Contains(app, "tr:nth-child(even)") {
		t.Error("tray zebra still present (R3)")
	}
}

// TestRowActionsCSSGuards pins the v0.6 row-actions CSS: the glyph button, the
// chevron menu, the destructive "asks first" note, and the phone 48px floor.
func TestRowActionsCSSGuards(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		".raglyph {",
		".ramenu-panel {",
		".ramenu-item.danger::after { content: \" — asks first\";",
		".raglyph { width: 44px; height: 44px; }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing row-actions rule %q", want)
		}
	}
}

// TestRowClickExcludesSummary pins the v0.6 row-navigation guard: a row opens
// its target (data-href/data-modal-row), but a click on a nested control —
// including the row-actions chevron <summary> — must NOT bubble into row
// navigation. Without the summary exclusion, a chevron click navigates the
// row instead of opening the menu.
func TestRowClickExcludesSummary(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/")
	for _, want := range []string{
		`e.target.closest('a,button,summary')`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("row-click handler missing the summary exclusion %q", want)
		}
	}
}
