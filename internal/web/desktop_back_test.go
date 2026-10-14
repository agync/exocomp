package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestDesktopBackAbsentOnTopLevel is the "up" guard from the desktop app-bar
// change: the back link renders only when the page names a parent, so the
// top-level /system page carries none.
func TestDesktopBackAbsentOnTopLevel(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if strings.Contains(page, `class="back"`) {
		t.Error("a top-level page must render no .back link in the app bar")
	}
}

// TestDesktopBackNamesParent is the "up to the named parent" guard: a page
// with BackHref and BackLabel set renders the worded app-bar back link to that
// parent.
func TestDesktopBackNamesParent(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "daily=7,weekly=4", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups/nas01/history")
	if code != http.StatusOK {
		t.Fatalf("history = %d, want 200", code)
	}
	want := `<a class="back" href="/system/backups" onclick="exoCloseNav()" aria-label="Back to targets">‹ <span>targets</span></a>`
	if !strings.Contains(page, want) {
		t.Errorf("the app-bar back link must render %q", want)
	}
}

// TestDesktopBackGlyphOnJobDetail is the back guard for the glyph form: the job
// detail page names its parent only in the aria-label, so the slot renders the
// joined chevron to /jobs.
func TestDesktopBackGlyphOnJobDetail(t *testing.T) {
	h := newHarness(t)
	job := seedTerminalJob(t, h, store.JobArgs{Argv: []string{"/bin/true"}})
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if code != http.StatusOK {
		t.Fatalf("job detail = %d, want 200", code)
	}
	if !strings.Contains(page, `<a class="back" href="/jobs" onclick="exoCloseNav()" aria-label="Back to jobs">`) {
		t.Error("the job detail app bar must render the back link to /jobs")
	}
}

// TestAppCSSRevealsDesktopBack is the CSS guard for the app-bar change: the
// phone-shell hide rule must no longer hide .back, so the one back element
// shows on the desktop bar too.
func TestAppCSSRevealsDesktopBack(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, `.appbar .hamburger, .appbar .brandlink { display: none; }`) {
		t.Error("the phone-shell hide rule must keep .hamburger and .brandlink only")
	}
	for _, gone := range []string{
		`.appbar .hamburger, .appbar .back, .appbar .brandlink { display: none; }`,
		`.appbar .back { display: none`,
	} {
		if strings.Contains(app, gone) {
			t.Errorf("app.css must not hide .back on desktop (%q present)", gone)
		}
	}
}
