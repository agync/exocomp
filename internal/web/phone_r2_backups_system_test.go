package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// phone_r2_backups_system_test.go — the phone-web round-2 backups/system gates
// (R7, R10, R11, R12). Layout is browser-verified at 390px; these pin the
// markup and the CSS the phone re-flow consumes. The R11 subtitle rule is the
// one Go-touched guard: a page class opts the system bar out, and jobs/events
// are asserted to keep theirs.

// phoneBlock returns the substring from the phone collapse container onward,
// so an assertion about a stub never trips on a desktop card's own copy.
func phoneBlock(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `<div class="sys-phone">`)
	if i < 0 {
		t.Fatal("missing the phone collapse container")
	}
	return page[i:]
}

// TestPhoneBackupsVerbRowSize is the R7 gate for the topbar verbs: `Restore`
// and `Add target` render at the row scale (40px, 14/500) in the backups verb
// group, not the 48px phone floor. The MUTATION — dropping the scoped rule —
// reds this test.
func TestPhoneBackupsVerbRowSize(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	want := "main .sys-bk-verbs.page-actions .btn { height: 40px; min-height: 40px; font: 500 14px var(--font-ui); }"
	if !strings.Contains(app, want) {
		t.Errorf("app.css missing the R7 backups verb row-size rule %q", want)
	}
}

// TestPhoneBackupsKVStackScoped is the R7 gate for the KIND/RETENTION/SCHEDULE
// posture cells: at 390px they stack key over value, left-aligned, so the value
// no longer hangs right with a void. The override is SCOPED to the three bk
// cells — the shared `.tablewrap.stackable td` rule stays untouched (its
// space-between layout is the desktop-independent contract other stackable
// tables read). The MUTATION — editing the shared rule, or dropping the scoped
// override — reds this test.
func TestPhoneBackupsKVStackScoped(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	lift := cssOf(t, h, ts, client, "lift.css")
	want := ".tablewrap.stackable .bk-kind, .tablewrap.stackable .bk-ret, .tablewrap.stackable .bk-sched { flex-direction:column; align-items:flex-start; justify-content:flex-start; gap:3px; }"
	if !strings.Contains(lift, want) {
		t.Errorf("lift.css missing the scoped R7 key-over-value rule %q", want)
	}

	// The SHARED stackable rule is untouched.
	app := cssOf(t, h, ts, client, "app.css")
	shared := "display: flex; justify-content: space-between; align-items: baseline;"
	if !strings.Contains(app, shared) {
		t.Error("the shared .tablewrap.stackable td rule must keep its space-between contract")
	}
}

// TestPhoneBackupsActionsLabelled is the R7 gate for the ACTIONS cell: the run
// verb is a LABELLED `Run now` button (not the bare ▷ glyph) and the chevron
// reads `More ▾` — both bar-size (36px) ghosts, right-aligned, gap 8. The
// MUTATION — restoring the glyph-only run verb or the wordless chevron — reds
// this test.
func TestPhoneBackupsActionsLabelled(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/exocomp", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != 200 {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	for _, want := range []string{
		`class="raglyph-label" aria-label="Queue backup — nas01" title="Queue backup — nas01">Run now</button>`,
		`<span class="rachev-word">More</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("backups ACTIONS missing the labelled verb %q", want)
		}
	}

	app := cssOf(t, h, ts, client, "app.css")
	for _, want := range []string{
		".rowacts { gap: 8px; justify-content: flex-end; }",
		".raglyph-label { height: 36px; padding: 0 12px; background: transparent; border-color: transparent; }",
		".rachev { width: auto; height: 36px; padding: 0 10px; gap: 6px; }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the R7 bar-size ghost rule %q", want)
		}
	}
}

// TestPhoneBackupsAddOneLink is the R7 gate for `add one`: the schedule cell's
// verbless hint carries a link affordance (accent ink) instead of reading as a
// stray grey word. The MUTATION — dropping the affordance class — reds this.
func TestPhoneBackupsAddOneLink(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "git-config", "git", "git@forge.lan:ops/exocomp-config.git", "", false)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != 200 {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	if !strings.Contains(page, `class="meta bk-add"`) {
		t.Error("the `add one` hint must carry the link-affordance class")
	}

	lift := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(lift, ".bk-sched .bk-add { color: var(--accent-text); }") {
		t.Error("lift.css must give `add one` the accent link color")
	}
}

// TestPhoneDrawerFooterIsAFlexSibling is the R10 gate for the launch drawer:
// the Queue run footer is a DIALOG SIBLING below the scroll body, not a sticky
// child of the form. The body is the flex scroll area, so its viewport ends
// where the footer begins and the last control (the `Run as allowed…` toggle
// row) can never hide under the footer at any scroll position. Scoped to the
// 719 block. The MUTATION — a sticky form-child footer — reds this.
func TestPhoneDrawerFooterIsAFlexSibling(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	i := strings.Index(app, "@media (max-width: 719px)")
	if i < 0 {
		t.Fatal("app.css missing the 719px phone block")
	}
	block := app[i:]
	for _, want := range []string{
		".modal.drawer .modal-body { padding-bottom: 12px; }",
		".modal.drawer .drawer-actions { flex: none;",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the 719 block must make the drawer footer a flex sibling of the scroll body %q", want)
		}
	}
	if strings.Contains(app, ".modal.drawer .drawer-actions { position: sticky;") {
		t.Error("the sticky drawer footer must be gone (the footer is a flex sibling)")
	}
}

// TestPhoneDrawerMoreHelper is the R10 gate for the options summary: the
// wrapping `More — path, inventory, …` sentence becomes a short `More` beside
// its chevron, with the field list as a 12px helper beneath it. The MUTATION —
// restoring the long summary sentence — reds this.
func TestPhoneDrawerMoreHelper(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != 200 {
		t.Fatalf("/sources/%s = %d, want 200", name, code)
	}
	if strings.Contains(page, "More — path, inventory, tags, secrets, run modes") {
		t.Error("the wrapping options summary sentence must be gone")
	}
	if !strings.Contains(page, `<p class="opts-help">path, inventory, tags, secrets, run modes</p>`) {
		t.Error("the field list must render as a 12px opts helper under the summary")
	}

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, ".opts-help { font: 400 12px var(--font-mono); color: var(--ink-500); margin: 4px 0 2px; }") {
		t.Error("app.css missing the R10 opts helper rule")
	}
}

// TestPhoneSystemPublicURLKey is the R11 gate for the identity strip's public
// URL cluster: on phone the key `public URL` shows (joined to the value with a
// ` · `) and `Change` is a bar-size ghost, not the filled 30px control. The
// MUTATION — hiding the label again, or keeping the filled Change — reds this.
func TestPhoneSystemPublicURLKey(t *testing.T) {
	h := newHarness(t)
	seedSystemShell(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != 200 {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `<span class="label">public URL</span>`) {
		t.Error("the identity strip must keep the real `public URL` key")
	}

	lift := cssOf(t, h, ts, client, "lift.css")
	for _, want := range []string{
		".sys-identity .pub .label { display:inline; }",
		`.sys-identity .pub .label::after { content:" ·"; }`,
		".sys-identity .change { height:36px; background:transparent; border-color:transparent; }",
	} {
		if !strings.Contains(lift, want) {
			t.Errorf("lift.css missing the R11 phone public-URL rule %q", want)
		}
	}
}

// TestPhoneSystemLegFigureHiddenAt48 is the R11 gate for the phone leg rows: a
// leg whose figure is the dim `—` stub drops the line entirely and the row
// reads one line at 48px; a real figure still renders (pinned separately by
// TestSystemPhoneLegRowsCarryState). The MUTATION — rendering the stub, or
// leaving the 56px floor — reds this.
func TestPhoneSystemLegFigureHiddenAt48(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != 200 {
		t.Fatalf("/system = %d, want 200", code)
	}
	if strings.Contains(phoneBlock(t, page), `>—</span>`) {
		t.Error("a phone leg row must not render the `—` figure stub")
	}

	lift := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(lift, "gap:2px 12px; align-items:center; min-height:48px;") {
		t.Error("the phone leg row must collapse to a 48px one-line row")
	}
}

// TestPhoneSystemBarSubtitleScoped is the R11 gate for the clipped bar
// subtitle: the system page opts out with a page class and the phone rule is
// scoped to it — jobs and events keep their bar meta. The MUTATION — a global
// `.appbar-meta` hide — reds the jobs/events guard.
func TestPhoneSystemBarSubtitleScoped(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, system := getBody(t, client, ts.URL+"/system")
	if !strings.Contains(system, `<div class="app page-system">`) {
		t.Error("the system page must opt into the phone bar-subtitle rule")
	}
	for _, path := range []string{"/jobs", "/events"} {
		_, page := getBody(t, client, ts.URL+path)
		if strings.Contains(page, "page-system") {
			t.Errorf("%s must not carry the system page class", path)
		}
	}

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, ".app.page-system .appbar-meta { display: none; }") {
		t.Error("app.css missing the scoped R11 bar-subtitle rule")
	}
	// Strip the scoped rule; nothing may remain that hides the bar subtitle.
	stripped := strings.ReplaceAll(app, ".app.page-system .appbar-meta { display: none; }", "")
	if strings.Contains(stripped, ".appbar-meta { display: none") {
		t.Error("the bar subtitle must never be hidden globally (jobs/events keep theirs)")
	}
}

// TestPhonePowerlineFirstSegInset is the R12 gate for the powerline's orphan
// indent: the first segment zeroes its own left padding instead of the shared
// `.powerline` gaining a bespoke inset. The MUTATION — dropping the rule —
// reds this.
func TestPhonePowerlineFirstSegInset(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, ".pl-seg.pl-first { padding-left: 0; }") {
		t.Error("app.css must zero the first powerline segment's left padding")
	}
}

// TestPhoneHamburgerBrandmarkOneTreatment is the R12 gate for the phone shell:
// the menu button drops its 1px border/shadow so it reads as the same bare
// treatment as the exocomp mark beside it. The MUTATION — leaving the border —
// reds this.
func TestPhoneHamburgerBrandmarkOneTreatment(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, ".appbar .hamburger { border: 0; box-shadow: none; }") {
		t.Error("the app-bar hamburger must share the bare brandmark treatment")
	}
}
