package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// ui_fixes_css_test.go — the UI-review CSS/layout close-out gates. Each test
// pins one defect measured in Chromium; the browser sees the served bytes, so
// the assertion reads the same stylesheet the page links.

// TestPhoneDrawerCoversWidth is FIX 1: the launch drawer sets width
// min(460px,100vw) but no max-width, so the UA dialog max-width caps it and a
// strip of the page shows on phone. A max-width:100vw on the base rule makes it
// full-width on phone; the 460px desktop drawer is unchanged (460 < 100vw).
func TestPhoneDrawerCoversWidth(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	rule := ruleDecl(app, ".modal.drawer {")
	if !strings.Contains(rule, "width: min(460px, 100vw)") {
		t.Fatalf("the base drawer lost its 460px/100vw width: %q", rule)
	}
	if !strings.Contains(rule, "max-width: 100vw") {
		t.Errorf(".modal.drawer needs max-width: 100vw so the UA dialog cap cannot clip it on phone: %q", rule)
	}
}

// TestPhoneEventCountsVisible is FIX 2: `.filters .count .k` is display:none
// globally and the 719 block hides `.full`, so on phone both count forms were
// hidden. The phone block must re-enable the compact k-form.
func TestPhoneEventCountsVisible(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, ".filters .count .full { display: none; }") {
		t.Fatal("the phone block must still hide the full count form")
	}
	if !strings.Contains(app, ".filters .count .k { display: inline; }") {
		t.Error("the phone block must re-enable the compact k count form")
	}
}

// TestSourcePlaybookRowWraps is FIX 3: a long nested playbook path overflows
// the flex row and pushes the chevron out. The path is a bare text node in the
// flex container, so the row itself carries overflow-wrap:anywhere (wrap, never
// truncate — the phone invariant) and the chevron is flex:none so it stays
// visible. Verified in Chromium: before, scrollWidth 414 > clientWidth 358 and
// the chevron sat past the row edge; after, the row wraps and the chevron is
// inside the row.
func TestSourcePlaybookRowWraps(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != 200 {
		t.Fatalf("/sources/%s = %d, want 200", name, code)
	}
	if !strings.Contains(page, `class="playbook-row"`) || !strings.Contains(page, `<span class="chev" aria-hidden="true">`) {
		t.Fatal("the source page must still render a playbook row with a chevron")
	}

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(ruleDecl(app, ".playbooklist .playbook-row {"), "overflow-wrap: anywhere") {
		t.Error("the playbook row must wrap a long path (overflow-wrap:anywhere), never truncate it")
	}
	if !strings.Contains(ruleDecl(app, ".playbooklist .playbook-row .chev {"), "flex: none") {
		t.Error("the playbook chevron must be flex:none so it never leaves the row")
	}
}

// TestRachevWordContrast is FIX 4: `.rachev` colours the chevron glyph AND the
// phone-only `.rachev-word` label; ink-600 is never text. The glyph keeps
// ink-600, the word reads ink-500 (the last AA text step).
func TestRachevWordContrast(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(ruleDecl(app, ".rachev {"), "color: var(--ink-600)") {
		t.Error("the chevron glyph keeps ink-600")
	}
	if !strings.Contains(ruleDecl(app, ".rachev-word {"), "color: var(--ink-500)") {
		t.Error(".rachev-word must read ink-500 (ink-600 is never text)")
	}
}

// TestColorSchemeThemes is FIX 5: the native form-control/scrollbar chrome must
// follow the theme. color-scheme:dark on the base :root, light in the OS-light
// media block and the explicit light override, dark on the explicit dark
// override (forced dark on a light OS must not show light scrollbars).
func TestColorSchemeThemes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	tokens := cssOf(t, h, ts, client, "tokens.css")
	if !strings.Contains(ruleDecl(tokens, ":root {"), "color-scheme: dark") {
		t.Error("tokens.css base :root must declare color-scheme: dark")
	}
	mediaAt := strings.Index(tokens, "@media (prefers-color-scheme: light)")
	if mediaAt < 0 {
		t.Fatal("tokens.css missing the prefers-color-scheme: light block")
	}
	if !strings.Contains(ruleDecl(tokens[mediaAt:], ":root {"), "color-scheme: light") {
		t.Error("the prefers-color-scheme: light block must declare color-scheme: light")
	}
	if !strings.Contains(ruleDecl(tokens, `:root[data-theme="light"] {`), "color-scheme: light") {
		t.Error("the explicit light override must declare color-scheme: light")
	}
	if !strings.Contains(ruleDecl(tokens, `:root[data-theme="dark"] {`), "color-scheme: dark") {
		t.Error("the explicit dark override must declare color-scheme: dark")
	}
}

// TestJobsTableResponsiveHalfScreen ensures .jobslist does not overflow on
// half-screen desktops: .jobslist has overflow-x: auto, the grid columns have
// flexible proportions that fit narrow viewports without collapsing the
// Playbook name, and .reason ellipsizes if long.
func TestJobsTableResponsiveHalfScreen(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, ".jobslist { padding-bottom: 8px; overflow-x: auto; min-width: 0; }") {
		t.Error(".jobslist missing overflow-x: auto; min-width: 0")
	}
	headRule := ruleDecl(app, ".jobshead {")
	if !strings.Contains(headRule, "grid-template-columns: 44px minmax(100px, 1.5fr) minmax(110px, 1.2fr) minmax(80px, 1fr) minmax(70px, 0.8fr);") {
		t.Errorf(".jobshead grid-template-columns must use flexible columns, got: %q", headRule)
	}
	rowRule := ruleDecl(app, ".jobs-row {")
	if !strings.Contains(rowRule, "grid-template-columns: 44px minmax(100px, 1.5fr) minmax(110px, 1.2fr) minmax(80px, 1fr) minmax(70px, 0.8fr);") {
		t.Errorf(".jobs-row grid-template-columns must use flexible columns, got: %q", rowRule)
	}
}
