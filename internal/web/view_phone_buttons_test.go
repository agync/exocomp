package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// view_phone_buttons_test.go — Wave 3 (m102/phone-buttons): the §2 button
// system on the phone form and its page consequences. The app bar's right
// slot is 36px ghosts with no filled accent (a); the page primary lives in
// the content .page-actions row (b); the backups topbar stacks its meta
// sentence and verb group and the ▾ holds: string is clean (c); Add secret /
// Configure sit inside .appbar-actions, the margin-left:auto right slot, as
// bar ghosts (d); the powerline's first segment draws no leading separator
// (e). Layout is not harness-testable — the integrator's browser pass
// verifies it at 390px.

// barSlot returns the rendered <header class="appbar">…</header> fragment, or
// "" when the page has none.
func barSlot(page string) string {
	i := strings.Index(page, `<header class="appbar">`)
	if i < 0 {
		return ""
	}
	j := strings.Index(page[i:], "</header>")
	if j < 0 {
		return page[i:]
	}
	return page[i : i+j]
}

// TestAppbarSlotNoFilledAccent is the §2 guard (a): the app bar's right slot
// renders no filled-accent block and its actions carry the bar shape. The Add
// verb is the bar-size ghost (`btn addbtn`); a glyph action (Raw log / Rerun)
// keeps the `.verb` shape — the accent Rerun is accent text on transparent,
// never a fill.
func TestAppbarSlotNoFilledAccent(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/secrets")
	if code != http.StatusOK {
		t.Fatalf("/secrets = %d, want 200", code)
	}
	bar := barSlot(page)
	if !strings.Contains(bar, `class="btn addbtn" data-modal-open="secrets-add">Add secret`) {
		t.Error("Add secret must render as the bar ghost (class=\"btn addbtn\"), not a filled accent")
	}
	if strings.Contains(bar, "primary") {
		t.Error("the secrets app bar's right slot must render no filled-accent block")
	}

	job := seedTerminalJob(t, h, store.JobArgs{Argv: []string{"/bin/true"}})
	code, jp := getBody(t, op, ts.URL+"/jobs/"+itoa64(job.ID))
	if code != http.StatusOK {
		t.Fatalf("job detail = %d, want 200", code)
	}
	jbar := barSlot(jp)
	if !strings.Contains(jbar, `class="verb" href="/jobs/`+itoa64(job.ID)+`/raw"`) {
		t.Error("the Raw log bar action must carry the .verb bar class")
	}
	if !strings.Contains(jbar, `class="verb primary"`) {
		t.Error("the Rerun accent glyph must stay a bar action")
	}
	if strings.Contains(jbar, "btn primary") || strings.Contains(jbar, "btn-compact primary") {
		t.Error("a bar text action must not render the filled accent")
	}

	_, app := getBody(t, op, ts.URL+"/assets/app.css")
	for _, want := range []string{
		".appbar .btn { background: transparent; }",
		".appbar .btn:hover, .appbar .btn:active { background: var(--surface-control); }",
		".appbar .btn-compact, .appbar .addbtn { min-height: 36px; height: 36px; }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the §2 bar ghost rule %q", want)
		}
	}
}

// TestPageActionsRowHoldsThePrimary is the §2 guard (b): a page primary lives
// in the content .page-actions row — the row scale, exactly one filled accent
// — not in the app bar.
func TestPageActionsRowHoldsThePrimary(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	i := strings.Index(page, `<span class="sys-bk-verbs page-actions">`)
	if i < 0 {
		t.Fatal("the backups verbs must be the content .page-actions row group")
	}
	j := strings.Index(page[i:], "</span>")
	if j < 0 {
		t.Fatal("the .page-actions verb group does not close")
	}
	verbs := page[i : i+j]
	if strings.Count(verbs, "btn primary") != 1 {
		t.Error("the .page-actions row must carry exactly one filled accent")
	}
	if !strings.Contains(verbs, `class="btn primary" data-modal-open="backups-add">Add target`) {
		t.Error("the one filled accent must be the Add target primary")
	}
	if strings.Contains(barSlot(page), "primary") {
		t.Error("the backups app bar must not render a filled accent")
	}

	_, app := getBody(t, op, ts.URL+"/assets/app.css")
	for _, want := range []string{
		".page-actions {",
		"flex-wrap: wrap; gap: 8px;",
		".page-actions .btn { min-height: 40px; height: 40px; }",
		".page-actions .btn.danger {",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the §2 page-actions row rule %q", want)
		}
	}
}

// TestBackupsTopbarStackedAndHoldsClean is the P7 guard (c): the backups
// topbar's meta sentence sits on its own line under the bar on phone, and the
// ▾ holds: footer string carries no bare `· ─ ·`.
func TestBackupsTopbarStackedAndHoldsClean(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	if !strings.Contains(page, "▾ holds: history · edit retention · disable · drill now · remove asks first") {
		t.Error("the ▾ holds: string must read …drill now · remove asks first without the stray `· ─ ·`")
	}
	if strings.Contains(page, "drill now · ─ ·") {
		t.Error("the ▾ holds: literal still carries the bare `· ─ ·`")
	}

	_, app := getBody(t, op, ts.URL+"/assets/app.css")
	for _, want := range []string{
		"main .sys-bkbar { flex-direction: column;",
		"main .sys-bk-meta { flex: none; width: 100%; }",
		"main .sys-bk-verbs { margin-left: 0;",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the P7 topbar stack rule %q", want)
		}
	}
}

// TestAddVerbSitsInAppbarActions is the P14 guard (d): Add secret / Configure
// render inside .appbar-actions — the right slot that carries margin-left:auto
// — as the bar-size ghost, never a filled 44px block in the bar.
func TestAddVerbSitsInAppbarActions(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/secrets")
	if code != http.StatusOK {
		t.Fatalf("/secrets = %d, want 200", code)
	}
	i := strings.Index(page, `<span class="appbar-actions">`)
	if i < 0 || !strings.Contains(page[i:], `class="btn addbtn" data-modal-open="secrets-add">Add secret`) {
		t.Error("Add secret must render inside .appbar-actions as the bar ghost")
	}
	if strings.Contains(barSlot(page), "primary") {
		t.Error("Add secret must not be a filled accent in the bar")
	}

	code, wp := getBody(t, op, ts.URL+"/workspaces")
	if code != http.StatusOK {
		t.Fatalf("/workspaces = %d, want 200", code)
	}
	wi := strings.Index(wp, `<span class="appbar-actions">`)
	if wi < 0 || !strings.Contains(wp[wi:], `class="btn addbtn" data-modal-open="ws-configure">Configure`) {
		t.Error("Configure must render inside .appbar-actions as the bar ghost")
	}

	_, app := getBody(t, op, ts.URL+"/assets/app.css")
	if !strings.Contains(app, ".appbar-actions { margin-left: auto; }") {
		t.Error("the bar's right slot must right-align with margin-left: auto")
	}
}

// TestPowerlineFirstSegmentNoSeparator is the P15 guard (e): the powerline's
// first segment is marked pl-first and the shared CSS drops its leading `|`
// separator, so no segment draws a rule before the first one.
func TestPowerlineFirstSegmentNoSeparator(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, op, ts.URL+"/")
	foot := strings.Index(page, `<footer class="powerline"`)
	if foot < 0 {
		t.Fatal("page missing the powerline footer")
	}
	first := strings.Index(page[foot:], `class="pl-seg`)
	if first < 0 {
		t.Fatal("powerline missing its first segment")
	}
	if !strings.HasPrefix(page[foot+first:], `class="pl-seg pl-first`) {
		t.Error("the powerline's first segment must carry the pl-first class")
	}
	if strings.Count(page, `class="pl-seg pl-first`) != 1 {
		t.Error("only the powerline's first segment may carry pl-first")
	}

	_, app := getBody(t, op, ts.URL+"/assets/app.css")
	if !strings.Contains(app, ".pl-seg.pl-first { box-shadow: none; }") {
		t.Error("app.css must drop the leading separator on the first segment")
	}
}
