package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// design_review_fixes_test.go — the v0.13 designer-review close-out gates
// (R1–R10 + D3 + the folded nits). Each test pins one finding from the
// designer's review of the built surfaces; the lane brief is the contract.

// cssOf fetches one served stylesheet asset for a CSS-content assertion.
func cssOf(t *testing.T, _ *harness, ts *httptest.Server, client *http.Client, asset string) string {
	t.Helper()
	code, css := getBody(t, client, ts.URL+"/assets/"+asset)
	if code != http.StatusOK {
		t.Fatalf("/assets/%s = %d, want 200", asset, code)
	}
	return css
}

// ---- R1 · ntfy card: the fan-out owns its grid; the ON figure is the topic ----

// TestNtfyCardFanOutOwnGrid is the R1 gate for the collapsed 3-column mini
// table: the fan-out rows render under their OWN three-column grid
// (minmax(0,1fr) 72px 120px) — never the fact-grid's two-column rule — and no
// fixed row height wraps a row's three cells. The mutation (falling back to
// the fact-grid columns, or a fixed 28px row) reds this test.
func TestNtfyCardFanOutOwnGrid(t *testing.T) {
	h := newHarness(t)
	seedOnChannel(t, h)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".leg-mini > div { display:grid; grid-template-columns:minmax(0,1fr) 72px 120px; gap:12px; align-items:center; height:24px;") {
		t.Error("fan-out rows lack their own minmax(0,1fr) 72px 120px grid at 24px")
	}
	if strings.Contains(css, "height:28px") {
		t.Error("fan-out rows still carry a fixed 28px height that wraps their cells")
	}

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, row := range []string{"approvals", "job failures", "dead-man tripped"} {
		if !strings.Contains(page, ">"+row+"</span>") {
			t.Errorf("fan-out missing the %q row", row)
		}
	}
}

// TestNtfyCardOnFigureIsTopicLabel is the R1 gate for the ON figure: the card
// figures the TOPIC LABEL (the one unsealed display string) at mono 19/600 —
// never a `—` on a healthy leg — with the `topic · self-hosted server` note.
func TestNtfyCardOnFigureIsTopicLabel(t *testing.T) {
	h := newHarness(t)
	seedOnChannel(t, h)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `<span class="value topic">exocomp-7f3a</span>`) {
		t.Error("ON figure is not the topic label at the 19/600 figure class")
	}
	if !strings.Contains(page, `>topic · self-hosted server</span>`) {
		t.Error("ON figure note missing the server class")
	}
	if strings.Contains(page, `<span class="value">—</span>`) {
		t.Error("a healthy ON leg must never figure a dash")
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".leg-figure .value.topic { font:600 19px/1.3 var(--font-mono);") {
		t.Error("topic figure lacks the mono 19/600 sizing")
	}
}

// ---- R2 · identity strip: the Change form rests closed ----

// TestSystemIdentityStripRestsClosed is the R2 GATE: at rest the Change form
// is CLOSED — the form carries `hidden`, the resting row is one line
// (`public URL · not set · Change`), and the Doctor · Kit glyphs sit at the
// strip's FAR RIGHT after the public-URL cluster. The mutation (rendering the
// form open at rest) reds the `hidden` assertion.
func TestSystemIdentityStripRestsClosed(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	// The form rests CLOSED: the server marks it hidden at rest; JS (and a
	// 422 re-render) opens it. A form that renders open at rest reds here.
	if !strings.Contains(page, `data-pubform hidden`) {
		t.Error("the Change form must rest closed (hidden) — it is permanently open")
	}
	// The resting row: value + the Change toggle (a quiet 30px submit that
	// expands the form on click and posts it JS-off).
	for _, want := range []string{
		"public URL",
		`<span class="val none">not set</span>`,
		`class="change" data-pub-toggle`,
		`form="pubform"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("resting strip missing %q", want)
		}
	}
	// Doctor · Kit glyphs at the strip's FAR RIGHT, after the public-URL
	// cluster (they floated above before).
	if i := strings.Index(page, `class="pub"`); i < 0 || i > strings.Index(page, `title="Doctor — run the self-check"`) {
		t.Error("public-URL cluster must render LEFT of the Doctor/Kit glyphs")
	}
	// The change submit is the ONE Change verb (no second one inside the form).
	if strings.Contains(page, `form="pubform"`) && !strings.Contains(page, `>Change</button>`) {
		t.Error("resting strip must render the Change verb")
	}
}

// TestSystemIdentityStripChangeFormOpensOn422 is the R2 mutation's other half:
// a refused public-URL submit re-renders /system at 422 with the form OPEN
// (data-open, no hidden) and the field refusal on the meta line — JS-off
// reaches the same bytes.
func TestSystemIdentityStripChangeFormOpensOn422(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/public-url",
		url.Values{"url": {"https://exocomp.example.ts.net/path"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("path-bearing public url = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, `data-pubform data-open`) {
		t.Error("the 422 re-render must open the Change form (data-open)")
	}
	if strings.Contains(body, `data-pubform hidden`) {
		t.Error("the 422 re-render must not keep the form hidden")
	}
	if !strings.Contains(body, "public URL must be at the origin root") {
		t.Error("422 re-render missing the field refusal")
	}
}

// ---- R3 · Restore drawer: From segmented, custom snapshot rows, kicker, (fail) span ----

// TestBackupRestoreFromSwitchSegmented is the R3 gate: the From switch wears
// the segmented shape (the same family as Threshold), NOT native radios
// floating above their labels, and its comparison lives on the meta line
// beneath.
func TestBackupRestoreFromSwitchSegmented(t *testing.T) {
	h := newHarness(t)
	seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	for _, want := range []string{
		`<div class="field-seg" role="radiogroup" aria-label="From">`,
		`value="snapshot"`, `value="replica"`,
		"> snapshot</label>", "> replica</label>",
		"verified by the drill",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("From switch missing %q", want)
		}
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".field-seg { display:flex; flex-wrap:nowrap;") {
		t.Error("the From segmented switch lacks the nowrap flex grid")
	}
}

// TestBackupRestoreSnapshotRowsCustom is the R3 gate: the snapshot group is a
// plain surface of custom 44px radio rows (16px dot, label inline) — NOT a
// browser-default <fieldset> with a border + legend — and the group meta reads
// BELOW the rows, not inline after them.
func TestBackupRestoreSnapshotRowsCustom(t *testing.T) {
	h := newHarness(t)
	seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if strings.Contains(page, "<fieldset") || strings.Contains(page, "<legend>") {
		t.Error("snapshot group must not be a browser-default fieldset with a legend")
	}
	for _, want := range []string{
		`<div class="snaps"`,
		`class="snap-row"`,
		`type="radio" name="snapshot"`,
		"newest first across every target · all 1 snapshots",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("snapshot group missing %q", want)
		}
	}
	// The group meta is a line BELOW the rows (its own block after them), not
	// inline after the last row's spans.
	if i := strings.Index(page, "newest first across every target"); i >= 0 {
		if !strings.Contains(page[i:], `</div>`) {
			t.Error("the group meta must sit in its own line below the rows")
		}
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".snap-row { display:grid; grid-template-columns:16px minmax(0,1fr);") ||
		!strings.Contains(css, "height:44px;") {
		t.Error("snapshot rows lack the 44px 16px-dot inline-label shape")
	}
}

// TestBackupRestoreWhatHappensKickerAndFailSpan is the R3 gate for WHAT
// HAPPENS: the kicker heads the three numbered lines (it was missing), and the
// loss's `(fail)` is a colour SPAN — a `fail` class — never a raw word in a Go
// string. The mutation (rendering `(fail)` as plain text) reds the bare-text
// assertion.
func TestBackupRestoreWhatHappensKickerAndFailSpan(t *testing.T) {
	h := newHarness(t)
	seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, `>WHAT HAPPENS</span>`) {
		t.Error("WHAT HAPPENS kicker missing above the numbered lines")
	}
	if !strings.Contains(page, `<span class="fail">(fail)</span>`) {
		t.Error("the loss word must render as a fail-coloured span")
	}
	if strings.Contains(page, `are lost (fail)`) {
		t.Error("(fail) leaked as raw text in the loss sentence")
	}
}

// ---- D3 · the restore verb stops pretending ----

// TestBackupRestoreShowsCommandNotRestore is the D3 gate: the drawer's primary
// reads `Show the restore command` (quiet, fail ring, still gated by the typed
// id) — never `Restore from #…`, a button that pretends the daemon can replace
// the store it serves.
func TestBackupRestoreShowsCommandNotRestore(t *testing.T) {
	h := newHarness(t)
	seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, `>Show the restore command</button>`) {
		t.Error("the pre-confirm verb must read 'Show the restore command'")
	}
	if strings.Contains(page, "Restore from #") || strings.Contains(page, ">Restore from replica<") {
		t.Error("the drawer must not render a 'Restore from …' button that pretends")
	}
	// The typed-confirm gate stays: the button carries the required id and the
	// ring-off JS keys off it.
	if !strings.Contains(page, `id="restore-submit" data-required-id="a3f9c2`) {
		t.Error("the typed-confirm gate (data-required-id) must survive")
	}
}

// TestBackupRestoreConfirmedBody is the D3 gate for the confirmed drawer: a
// valid typed confirm changes the body to the RUN ON THE HOST block — the
// kicker in fail, the recovery-kit command in a code block with a Copy verb,
// and the sentence that the daemon cannot replace the store it serves. The
// mutation (flashing instead of the body) reds the block assertions.
func TestBackupRestoreConfirmedBody(t *testing.T) {
	h := newHarness(t)
	id := seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/backups/restore",
		url.Values{"from": {"snapshot"}, "snapshot": {id}, "confirm": {id}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid restore confirm = %d, want 200 (drawer re-render)", resp.StatusCode)
	}
	for _, want := range []string{
		`exoModalOpen("restore-backups")`,
		">RUN ON THE HOST<",
		">exocomp restore</pre>",
		`class="copy-i"`,
		"the daemon cannot replace the store it serves · this is the recovery kit path",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirmed body missing %q", want)
		}
	}
	if strings.Contains(body, "confirmed —") {
		t.Error("the confirmed state must not flash a pretend success")
	}
	if strings.Contains(body, ">Show the restore command</button>") {
		t.Error("a confirmed drawer must not still offer the pre-confirm verb")
	}
}

// TestBackupRestoreConfirmedBodyReplica is the D3 replica face: the confirmed
// body names `exocomp restore --from-litestream` and the same sentence.
func TestBackupRestoreConfirmedBodyReplica(t *testing.T) {
	h := newHarness(t)
	seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/backups/restore",
		url.Values{"from": {"replica"}, "confirm": {"replica"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replica confirm = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, ">exocomp restore --from-litestream</pre>") {
		t.Error("replica confirmed body missing the --from-litestream command")
	}
	if !strings.Contains(body, "this is the recovery kit path") {
		t.Error("replica confirmed body missing the recovery-kit sentence")
	}
}

// ---- R4 · Backups card: figure, short ids, disabled chip ----

// TestBackupsCardFigureAndSentence is the R4 gate for the card's FIGURE slot:
// the newest SUCCEEDED-run age figures with the ONE-rule note — the same rule
// the Dead-man card quotes — and the sentence shortens to the DR path.
func TestBackupsCardFigureAndSentence(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 4*time.Minute)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, ">4m</span>") || !strings.Contains(page, ">since the newest SUCCEEDED run</span>") {
		t.Error("backups card figure missing the newest SUCCEEDED-run age and note")
	}
	if !strings.Contains(page, "any one target recovers the system · restore is the DR path") {
		t.Error("backups card sentence did not shorten to the DR path")
	}
	if strings.Contains(page, "freshness ok &lt; 24h") {
		t.Error("the freshness ladder still rides the card sentence")
	}
}

// TestBackupsCardShortSnapshotID is the R4 gate: the snapshot id truncates to
// the 12-char short form the list already uses (`#18047422d234`) — the full
// 64-hex id never reaches /system or the /system/backups LAST RUN cell.
func TestBackupsCardShortSnapshotID(t *testing.T) {
	h := newHarness(t)
	const full = "18047422d234567890abcdef1234567890abcdef1234567890abcdef1234"
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, full, 1_200_000_000, 4*time.Minute)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, path := range []string{"/system", "/system/backups"} {
		code, page := getBody(t, client, ts.URL+path)
		if code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, code)
		}
		if strings.Contains(page, full) {
			t.Errorf("%s leaked the full 64-hex snapshot id", path)
		}
	}
	// The list's LAST RUN cell renders the short id.
	_, list := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(list, "#18047422d234") {
		t.Error("the list must render the 12-char short snapshot id")
	}
}

// TestBackupsCardDisabledTargetIsNotFailure is the R4 gate for the chip rule:
// a disabled target is not a failure and not a gap — an OK enabled target
// keeps the chip OK, and the mini table reads `DISABLED · never ran`.
func TestBackupsCardDisabledTargetIsNotFailure(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 4*time.Minute)
	seedTarget(t, h, "git-config", "git", "git@forge.lan:ops/exocomp-config.git", "", false)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	// The chip is scoped to the backups card (the dead-man card legitimately
	// reads NONE on this fixture).
	i := strings.Index(page, `<section class="leg" id="backups"`)
	if i < 0 {
		t.Fatal("backups card missing")
	}
	card := page[i:]
	if !strings.Contains(card, ">OK</span>") {
		t.Error("chip must stay OK with a disabled target present")
	}
	if strings.Contains(card, `>NONE<`) {
		t.Error("a disabled target must not push the chip to NONE")
	}
	if !strings.Contains(card, ">DISABLED</span>") || !strings.Contains(card, ">never ran</span>") {
		t.Error("the mini table must read the disabled target's DISABLED · never ran")
	}
}

// ---- R5 · Dead-man NONE ring is wait, never accent ----

// TestDeadmanNoneRingIsWait pins R5: the NONE card wears the wait (amber) ring
// and a wait chip dot — accent stays reserved for the Arm primary inside it,
// and only fail rings are red. A regression to accent reds this test.
func TestDeadmanNoneRingIsWait(t *testing.T) {
	h := newHarness(t)
	seedBackup(t, h, "nas01", store.BackupSucceeded, time.Now().Add(-16*time.Hour).UTC())
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `<section class="leg" id="deadman" aria-label="Dead-man" data-state="wait">`) {
		t.Error("NONE card must wear the wait ring")
	}
	if !strings.Contains(page, `<span class="chip wait"><span class="dot"></span>NONE</span>`) {
		t.Error("NONE chip must carry the wait dot")
	}
	if strings.Contains(page, `id="deadman" aria-label="Dead-man" data-state="accent"`) {
		t.Error("the dead-man ring must never read accent")
	}
}

// ---- R6 · the litestream leg names itself + its word, not the retired rpo metric ----

// TestPowerlineLitestreamWord is the R6 gate: with a set target and no
// sidecar the litestream leg card reads `NO SIDECAR` in fail — the leg names
// itself and its word, never the retired `rpo` metric. The /system footer
// segment is gone with m96; the card is the loud surface.
func TestPowerlineLitestreamWord(t *testing.T) {
	h := newHarness(t)
	noSidecar(h)
	setReplicaTarget(t, h, "/mnt/nas/exocomp-replica")
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `data-state="fail"`) || !strings.Contains(page, "NO SIDECAR") {
		t.Error("the litestream leg card must read fail 'NO SIDECAR'")
	}
	if strings.Contains(page, ">rpo ") {
		t.Error("the rpo metric name is retired from the self-care page")
	}
}

// ---- R7 · /system/backups header, footer, ▾ history, two-line rows ----

// TestBackupsHeaderMetaVerbsAndFooter is the R7 gate: meta left, verbs right
// (verbs push with margin-left:auto), the card's repeated SYSTEM / BACKUPS
// kicker dropped (the topbar breadcrumbs), and the footer's two sentences
// joined by a ` · ` (the ladder's join dot is the ::before, so its span does
// not carry it in markup).
func TestBackupsHeaderMetaVerbsAndFooter(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	if !strings.Contains(page, ">System / Backups · 1 target · any one recovers the system") {
		t.Error("the topbar meta must carry the breadcrumb and the singular targets line")
	}
	if strings.Contains(page, `class="kicker">System / Backups</div>`) {
		t.Error("the card must not repeat the SYSTEM / BACKUPS kicker")
	}
	if !strings.Contains(page, "remove asks first</span> <span class=\"right\">freshness ok &lt; 24h") {
		t.Error("the footer's ladder span must not carry the leading join dot in markup")
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".sys-bk-verbs { margin-left:auto;") {
		t.Error("the header verbs must push right with margin-left:auto")
	}
	if !strings.Contains(css, ".sys-bkbar { display:flex; align-items:center; gap:24px;") {
		t.Error("the header bar must lay meta left / verbs right with a 24px gap")
	}
}

// TestBackupsRowMenuHasHistory is the R7 gate: the ▾ list gains `history`
// (the TARGET link stays the second way in).
func TestBackupsRowMenuHasHistory(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, `href="/system/backups/nas01/history">History</a>`) {
		t.Error("the row ▾ must carry the history view")
	}
}

// TestBackupsListRowsTwoLine is the R7 gate for the two-line 64px rows: the
// LAST RUN cell stacks the drill outcome on its second line (`drill —` when
// never drilled, `drill 3d ago ok` after a succeeded drill).
func TestBackupsListRowsTwoLine(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, "drill —") {
		t.Error("a never-drilled row's LAST RUN second line must read 'drill —'")
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".bk-lastrun {") || !strings.Contains(css, ".lr-meta {") {
		t.Error("the LAST RUN cell lacks the two-line stack")
	}
	if !strings.Contains(css, ".bk-row { min-height:64px; align-items:center;") {
		t.Error("the backups rows must be min-height 64px, never a fixed height")
	}
	if strings.Contains(css, ".bk-row { height:64px;") {
		t.Error("a fixed row height clips the two-line cells — use min-height")
	}
}

// TestBackupsLastRunJobLinksOnly is the folded nit: only the `job #N` reads as
// a link in the LAST RUN cell — not the whole line.
func TestBackupsLastRunJobLinksOnly(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	job := seedRunningRun(t, h, nas)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, `href="/jobs/`+itoa64(job)+`">job #`+itoa64(job)+`</a>`) {
		t.Error("the LAST RUN cell must link the job id, not the line")
	}
	if !strings.Contains(page, `>job #`+itoa64(job)+`</a>`) {
		t.Error("the job id must render as the linked fragment")
	}
}

// ---- R8 · target history page ----

// TestBackupHistoryBackLinkAndCopy is the R8 gate: `Back to targets` becomes
// a `‹ targets` quiet link at the topbar's left with the meta full width, the
// meta names drilled snapshots, and the footer joins its sentences with ` · `.
func TestBackupHistoryBackLinkAndCopy(t *testing.T) {
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
	if !strings.Contains(page, `>‹ targets</a>`) {
		t.Error("the history page topbar must render the '‹ targets' back link")
	}
	if strings.Contains(page, ">Back to targets</a>") {
		t.Error("the wrapping 'Back to targets' verb must be gone")
	}
	// The topbar meta names drilled snapshots (the footer keeps the forget
	// gate's "absent on the newest and drilled" copy — that is the A12 rule).
	i := strings.Index(page, `class="sys-bk-meta"`)
	if i < 0 || !strings.Contains(page[i:], "forget removes a snapshot through the one queue · drilled snapshots") {
		t.Error("the meta must read '…drilled snapshots', not 'absent on the newest and drilled'")
	}
	if !strings.Contains(page, "forget asks first · absent on the newest and drilled snapshots</span> <span class=\"right\">a drill verifies") {
		t.Error("the footer's ladder span must not carry the leading join dot in markup")
	}
}

// TestBackupHistoryNoVerbRowDash is the R8 gate: the ACTIONS cell of a
// no-verb row (the newest/drilled snapshot) renders `—` — the column stays for
// the next snapshot's ▾.
func TestBackupHistoryNoVerbRowDash(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "daily=7,weekly=4", true)
	seedRun(t, h, nas, store.BackupSucceeded, "c04b91dddddd222222222222", 1_100_000_000, 8*24*time.Hour)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups/nas01/history")
	if code != http.StatusOK {
		t.Fatalf("history = %d, want 200", code)
	}
	// The newest (no-verb) row's ACTIONS cell renders a dash.
	if !strings.Contains(page, `data-label="ACTIONS" class="actions-col"><span class="meta">—`) {
		t.Error("the no-verb row's ACTIONS cell must render a '—', not stay empty")
	}
}

// ---- R9 · ntfy drawer ----

// TestNtfyDrawerDetailSwitchNowrap is the R9 gate: the Detail switch's segments
// never wrap — the control is a nowrap flex row, the same fault family R1/R3
// fix.
func TestNtfyDrawerDetailSwitchNowrap(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".field-switch { display:flex; flex-wrap:nowrap;") {
		t.Error("the Detail switch must be a nowrap flex row")
	}
}

// TestNtfyDrawerTopicRowShowsLabel is the R9 gate: the ntfy/topic presence row
// shows its LABEL — the one unsealed display string — never a bare empty dot
// run.
func TestNtfyDrawerTopicRowShowsLabel(t *testing.T) {
	h := newHarness(t)
	seedOnChannel(t, h)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy = %d, want 200", code)
	}
	if !strings.Contains(page, `>ntfy/topic</span>`) {
		t.Error("the topic presence row is missing")
	}
	if !strings.Contains(page, `>exocomp-7f3a</span>`) {
		t.Error("the topic row must show its label (the one unsealed display string)")
	}
}

// TestNtfyDrawerPushCard is the R9 gate: WHAT A PUSH SAYS is a sample PUSH —
// the phone-push card shape (surface-rail, 12px pad, title 13/600, body 12)
// with `[ Approve ] [ Reject ]` as 30px quiet buttons, not three bare lines.
func TestNtfyDrawerPushCard(t *testing.T) {
	h := newHarness(t)
	seedOnChannel(t, h)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy = %d, want 200", code)
	}
	if !strings.Contains(page, `class="push-sample"`) {
		t.Error("WHAT A PUSH SAYS must render as the phone-push card")
	}
	if !strings.Contains(page, `>Approve</button>`) || !strings.Contains(page, `>Reject</button>`) {
		t.Error("the sample's actions must be Approve/Reject quiet buttons")
	}
	if strings.Contains(page, "[ Approve ]") {
		t.Error("the sample actions must not be literal bracket text")
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".push-sample { background:var(--surface-rail);") {
		t.Error("the push card must ride the surface-rail")
	}
}

// TestNtfyDrawerHeadClosePlacement is the R9 gate for the ×: the close sits in
// the drawer head's top-right (a flex row), never wrapped under the title.
func TestNtfyDrawerHeadClosePlacement(t *testing.T) {
	h := newHarness(t)
	srv := serverWithNtfy(t, h, &fakeNtfy{st: onPresence()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/ntfy")
	if code != http.StatusOK {
		t.Fatalf("/system/ntfy = %d, want 200", code)
	}
	if !strings.Contains(page, `<div class="drawer-head">`) {
		t.Error("the drawer head is missing")
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".ntfy-drawer .drawer-head { display:flex; align-items:center; justify-content:space-between;") {
		t.Error("the drawer head must row-lay the close at the top right")
	}
}

// ---- R10 · leg pages carry the System breadcrumb ----

// TestLegPageTopbarTitles is the R10 gate: every /system leg page's topbar
// reads `System · {Leg}` — ntfy already did; litestream was empty, deadman and
// backups named the leg only.
func TestLegPageTopbarTitles(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	cases := []struct {
		path, title string
	}{
		{"/system/deadman", "System · Dead-man"},
		{"/system/litestream", "System · Litestream"},
		{"/system/backups", "System · Backups"},
	}
	for _, tc := range cases {
		code, page := getBody(t, client, ts.URL+tc.path)
		if code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", tc.path, code)
		}
		if !strings.Contains(page, `<h1 class="appbar-title">`+tc.title+`</h1>`) {
			t.Errorf("%s topbar title missing %q", tc.path, tc.title)
		}
	}
}

// TestBackupHistoryTopbarTitle is R10 for the history page: the breadcrumb
// keeps the target (`System · Backups · {target}`).
func TestBackupHistoryTopbarTitle(t *testing.T) {
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
	if !strings.Contains(page, `<h1 class="appbar-title">System · Backups · nas01</h1>`) {
		t.Error("history topbar title must read 'System · Backups · nas01'")
	}
}

// ---- nits ----

// TestDeadmanArmHeartbeatMeta regains the sealing sentence's lead: the Arm
// dialog's heartbeat meta states when the daemon pings and where the value is
// printed, before the sealing sentence.
func TestDeadmanArmHeartbeatMeta(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system")
	if !strings.Contains(page, "pinged after every succeeded backup · printed on the recovery kit · sealed as deadman/heartbeat_url on Arm, then never shown here again") {
		t.Error("the Arm dialog heartbeat meta lost the ping/print sentence")
	}
}

// TestPhonePowerlineScrolls is the phone-powerline nit: at ≤720 the global
// bottom powerline is a single overflow-x row — all segments or none, never a
// wrapped stack (app.css owns the mobile .powerline block; the /system page
// level strip is gone with m96).
func TestPhonePowerlineScrolls(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, ".powerline{position:sticky;bottom:0;height:36px;font-size:12px;") {
		t.Error("the phone powerline must be a single scrollable row")
	}
	if !strings.Contains(app, "overflow-x:auto") {
		t.Error("the phone powerline row must scroll horizontally")
	}
}

// TestPowerlineSeparators is the m96 separator GATE: the bottom powerline
// adopts the /system page-level look — the pl-seg carries the `|` hairline
// (padding:0 14px + inset 1px 0 0) and the powerline's gap drops to 0 so the
// hairline is the separator. Dropping either reds this test.
func TestPowerlineSeparators(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, "box-shadow: inset 1px 0 0 var(--hairline-faint);") {
		t.Error("the pl-seg must carry the inset hairline separator")
	}
	if !strings.Contains(app, "align-items: center; gap: 0;") {
		t.Error("the powerline gap must be 0 so the hairline separates")
	}
}

// ---- R2b · the Change form lives inside the strip; closed renders nothing ----

// TestSystemPubFormInsideStrip is the R2b GATE (BLOCK): the Change form is a
// flex child INSIDE the identity strip — the closed page renders no stray URL
// input BELOW the strip — and the closed form's `hidden` beats the .pubform
// display rule (the ghost was `[hidden]` losing to display:flex). The
// mutation (the form back below the strip, or the CSS hidden guard dropped)
// reds here.
func TestSystemPubFormInsideStrip(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	i := strings.Index(page, `id="pubform"`)
	if i < 0 {
		t.Fatal("the Change form is missing")
	}
	// The form and its URL input render INSIDE the strip, before the strip's
	// closing </section> — never a stray sibling below it (the ghost).
	stripEnd := strings.Index(page, "</section>")
	if stripEnd < 0 || i > stripEnd {
		t.Error("the Change form must live inside the identity strip, never below it")
	}
	if ui := strings.Index(page, `id="pub-url"`); ui < 0 || ui > stripEnd {
		t.Error("the closed page must render no stray URL input below the strip")
	}
	// The closed form's hidden attribute beats the .pubform display rule.
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".pubform[hidden] { display:none !important") {
		t.Error("the closed form's hidden must beat the .pubform display rule")
	}
}

// ---- R1b · the backups mini table owns its grid ----

// TestBackupsCardMiniTableOwnGrid is the R1b gate for the Backups card's mini
// table: name · state · age as its OWN three-column grid
// (minmax(0,1fr) 88px 64px, 24px rows) — the same row shape the fan-out uses,
// never the two-column fact-grid or auto-justified columns.
func TestBackupsCardMiniTableOwnGrid(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 4*time.Minute)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".bk-card-row { display:grid; grid-template-columns:minmax(0,1fr) 88px 64px; gap:12px; align-items:center; height:24px;") {
		t.Error("backups mini rows lack their own minmax(0,1fr) 88px 64px grid at 24px")
	}

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	i := strings.Index(page, `<section class="leg" id="backups"`)
	if i < 0 {
		t.Fatal("backups card missing")
	}
	card := page[i:]
	for _, want := range []string{
		`class="bk-card-row"`,
		">nas01</span>",
		">SUCCEEDED</span>",
		">4m</span>",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("backups mini row missing %q", want)
		}
	}
}

// ---- R7b · /system/backups column map and row height ----

// TestBackupsRowsColumnMap is the R7b gate: each posture cell owns its
// content — KIND (kind + path), RETENTION (policy + consequence), SCHEDULE
// (no schedule + add-one link) — the rows pad 12px 0, and the posture cells
// keep their two lines nowrapped (the review saw a misassigned map and 90px
// rows).
func TestBackupsRowsColumnMap(t *testing.T) {
	h := newHarness(t)
	nas := seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	seedRun(t, h, nas, store.BackupSucceeded, "a3f9c2abcdef1234567890ab", 1_200_000_000, 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	for _, want := range []string{
		`<td data-label="KIND" class="bk-kind"><span class="bk-cell"><span class="mono">restic_local</span> <span class="mono uri" title="/var/backups/nas01">/var/backups/nas01</span></span></td>`,
		`<td data-label="RETENTION" class="bk-ret"><span class="bk-cell"><span class="mono ok">daily 7 · weekly 4</span> <span class="meta">prunes after run</span></span></td>`,
		`<td data-label="SCHEDULE" class="bk-sched"><span class="bk-cell"><span class="meta wait-text">no schedule</span> <a href="/schedules" class="meta">add one</a></span></td>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("backups row cell missing %q", want)
		}
	}
	// SCHEDULE's content stays in its own cell — never the retention cell.
	if strings.Contains(page, `bk-ret">no schedule`) {
		t.Error("the schedule content leaked into the retention cell")
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".bk-table tbody tr.bk-row td { padding:12px 0;") {
		t.Error("backups rows must pad 12px 0, not stack to a 90px row")
	}
	if !strings.Contains(css, ".bk-kind .mono { white-space:nowrap; overflow:hidden; text-overflow:ellipsis; max-width:180px; font-size:13px;") {
		t.Error("the KIND cell must keep its line nowrapped at mono 13 with the path ellipsis")
	}
	if !strings.Contains(css, ".bk-ret .mono, .bk-sched .mono { white-space:nowrap; }") {
		t.Error("the RETENTION and SCHEDULE lines must stay nowrapped")
	}
	if strings.Contains(css, ".bk-ret .mono, .bk-sched .mono { white-space:nowrap; overflow:hidden") {
		t.Error("only the KIND path may ellipsize — the text cells never clip")
	}
	// The footer splits to TWO lines: the verbs left, the freshness ladder
	// right, in a flex row that wraps at 1280.
	if !strings.Contains(css, ".sys-bk-footer { display:flex; flex-wrap:wrap;") {
		t.Error("the footer must be a wrapping flex row so the ladder splits onto its own line")
	}
	if !strings.Contains(css, ".sys-bk-footer .right { margin-left:auto;") {
		t.Error("the freshness ladder must sit at the footer's right")
	}
}

// ---- R7c · the backups row unfixes: min-height, cell sizes, plural, footer ----

// TestBackupsMetaCountPluralises is the R7c gate for the topbar meta line:
// ONE target reads `1 target`, several read `N targets` — never a bare
// `N targets` for a single row.
func TestBackupsMetaCountPluralises(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, "System / Backups · 1 target ·") {
		t.Error("a single target must read '1 target', not '1 targets'")
	}
	seedTarget(t, h, "nas02", "restic_local", "/var/backups/nas02", "", true)
	_, page = getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, "System / Backups · 2 targets ·") {
		t.Error("multiple targets must read 'N targets'")
	}
}

// TestBackupsRowMinHeightNotFixed is the R7c gate for the row itself: the
// rows are min-height 64px with centered cells and 12px 0 cell padding —
// NEVER a fixed height or a clip on the row (the KIND path ellipsis is the
// only overflow allowed, and it lives in the cell, not the row).
func TestBackupsRowMinHeightNotFixed(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".bk-row { min-height:64px; align-items:center; }") {
		t.Error("the row must be min-height 64px with centered cells, not a fixed height")
	}
	if strings.Contains(css, ".bk-row { height:64px;") {
		t.Error("a fixed row height clips the two-line cells — use min-height")
	}
	if !strings.Contains(css, ".bk-table tbody tr.bk-row td { padding:12px 0;") {
		t.Error("the row's cells must pad 12px 0")
	}
}

// TestBackupsKindCellSizes is the R7c gate for the KIND cell: the kind line
// is mono 13, the path line drops to 12px ink-400 with the cell ellipsis.
func TestBackupsKindCellSizes(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".bk-kind .uri { font-size:12px; color:var(--ink-400);") {
		t.Error("the KIND path must render at 12px ink-400")
	}
	if strings.Contains(css, ".bk-kind .uri { font-size:11px;") {
		t.Error("the KIND path must not fall back to the 11px ink-500 sub-line")
	}
}

// TestBackupsRowCellsStayTableCells is the R7c fourth-pass gate for the row
// height: the KIND/RETENTION/SCHEDULE tds stay table-cells (their rule carries
// no display:flex — a flex td escapes the table's row layout and the row
// balloons to 132px), and the two-line column moves onto an inner .bk-cell
// wrapper. The row keeps min-height 64px.
func TestBackupsRowCellsStayTableCells(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	// Each posture cell wraps its two lines in the inner .bk-cell so the td
	// itself stays a table-cell (the DOM proxy for the row's shared height).
	for _, want := range []string{
		`<td data-label="KIND" class="bk-kind"><span class="bk-cell"><span class="mono">restic_local</span> <span class="mono uri" title="/var/backups/nas01">/var/backups/nas01</span></span></td>`,
		`<td data-label="RETENTION" class="bk-ret"><span class="bk-cell"><span class="mono ok">daily 7 · weekly 4</span> <span class="meta">prunes after run</span></span></td>`,
		`<td data-label="SCHEDULE" class="bk-sched"><span class="bk-cell"><span class="meta wait-text">no schedule</span> <a href="/schedules" class="meta">add one</a></span></td>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("backups row cell missing the .bk-cell wrapper: %q", want)
		}
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".bk-cell { display:flex; flex-direction:column; gap:4px; align-items:flex-start; min-width:0; }") {
		t.Error("the two-line column must live on the inner .bk-cell wrapper")
	}
	if !strings.Contains(css, ".bk-kind, .bk-ret, .bk-sched { vertical-align:middle; min-width:0; }") {
		t.Error("the posture tds must stay table-cells (vertical-align middle)")
	}
	if strings.Contains(css, ".bk-kind, .bk-ret, .bk-sched { display:flex") {
		t.Error("display:flex on the posture tds escapes the table's row layout")
	}
	if !strings.Contains(css, ".bk-row { min-height:64px; align-items:center; }") {
		t.Error("the row must keep min-height 64px")
	}
}

// ---- round-4 · the 08 recapture nits ----

// TestBackupsFooterStacksNarrow is the nit (1) gate: at ≤1440 the freshness
// ladder drops onto its OWN line — the footer stacks the two rows in a column
// with a 4px row gap (the wrapped rows were crammed at gap 0), and the ladder
// drops its join dot with it (the "·" is a ::before, not markup). The
// mutation (the media query gone, or the base row gap back to 0) reds here.
func TestBackupsFooterStacksNarrow(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".sys-bk-footer { display:flex; flex-wrap:wrap; align-items:center; gap:4px 8px;") {
		t.Error("the footer's wrapped rows must carry a 4px row gap, never gap 0")
	}
	if !strings.Contains(css, `.sys-bk-footer .right::before { content:"·";`) {
		t.Error("the footer's join dot must be the ladder's ::before so the stack can drop it")
	}
	if !strings.Contains(css, "@media (max-width:1440px)") {
		t.Error("the footer must stack the rows in a ≤1440 media query")
	}
	if mqStart := strings.Index(css, "@media (max-width:1440px)"); mqStart >= 0 {
		mq := css[mqStart:]
		if !strings.Contains(mq, ".sys-bk-footer { flex-direction:column") {
			t.Error("the ≤1440 footer must stack the two rows in a column")
		}
		if !strings.Contains(mq, ".sys-bk-footer .right::before { content:none;") {
			t.Error("the stacked ladder must drop its join dot")
		}
	}
	// The ladder's span carries no leading join dot in markup — the ::before
	// is the join, dropped when stacked.
	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, `<span class="right">freshness ok &lt; 24h`) {
		t.Error("the ladder's span must not carry the leading join dot in markup")
	}
}

// TestBackupsPostureSecondLinesTwelveMono is the nit (2) gate: the posture
// cells' SECOND line is 12px MONO ink-400 across all three — the KIND path
// already is, the RETENTION and SCHEDULE meta were 11px sans. The mutation
// (back to 11px, or the sans --type-body) reds here.
func TestBackupsPostureSecondLinesTwelveMono(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, `.bk-ret .meta, .bk-sched .meta { font:400 12px/1.4 var(--font-mono); font-variant-numeric:tabular-nums; color:var(--ink-400); }`) {
		t.Error("the RETENTION and SCHEDULE meta must be 12px mono ink-400, never the 11px sans sub-line")
	}
	if strings.Contains(css, ".bk-ret .meta { font-size:11px") {
		t.Error("the RETENTION meta must not fall back to the 11px sub-line")
	}
	if strings.Contains(css, ".bk-sched .meta { font-size:11px") {
		t.Error("the SCHEDULE meta must not fall back to the 11px sub-line")
	}
	if strings.Contains(css, ".bk-ret .meta { font-size:11px; color:var(--ink-500); }") || strings.Contains(css, ".bk-sched .meta { font-size:11px; color:var(--ink-500); }") {
		t.Error("the 11px ink-500 sub-line rules must be gone")
	}
}

// TestBackupsNeverRanMetaSized is the nit (3) gate: a never-run target's LAST
// RUN shows `never ran` at the META size — the word renders inside the
// .lr-meta span, never as the oversized .lr-text line. The mutation (the word
// back in the line slot) reds here.
func TestBackupsNeverRanMetaSized(t *testing.T) {
	h := newHarness(t)
	seedTarget(t, h, "nas01", "restic_local", "/var/backups/nas01", "", true)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("/system/backups = %d, want 200", code)
	}
	if !strings.Contains(page, `<span class="lr-meta">never ran`) {
		t.Error("a never-run row's 'never ran' must render inside the .lr-meta span at the meta size")
	}
	if strings.Contains(page, `lr-text">never ran`) || strings.Contains(page, `lr-text dim">never ran`) {
		t.Error("'never ran' must not render as the oversized .lr-text line")
	}
	// The first line holds no word for a never-run target — the meta line owns
	// the word.
	if !strings.Contains(page, `<span class="lr-line">`) {
		t.Error("the LAST RUN cell must keep its two-line stack")
	}
}

// ---- R8b · the history card drops the repeated kicker ----

// TestBackupHistoryCardDropsKicker is the R8b gate: the history card drops
// its SYSTEM/BACKUPS/target kicker — the topbar breadcrumb already carries it
// — and keeps the title.
func TestBackupHistoryCardDropsKicker(t *testing.T) {
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
	if strings.Contains(page, `<div class="kicker">System / Backups / nas01</div>`) {
		t.Error("the history card must not repeat the topbar's SYSTEM / BACKUPS / target kicker")
	}
	if !strings.Contains(page, `<h3>Snapshot history</h3>`) {
		t.Error("the history card must keep its title")
	}
}

// ---- round-2 nits ----

// TestDeadmanLastGoodTwelveCharID is the nit: the dead-man card's last-good
// snapshot id is the 12-char cut the rest of the surface uses, never 6.
func TestDeadmanLastGoodTwelveCharID(t *testing.T) {
	h := newHarness(t)
	seedBackup(t, h, "nas01", store.BackupSucceeded, time.Now().Add(-16*time.Hour).UTC())
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system")
	if !strings.Contains(page, ">16h 00m ago · nas01 #a3f9c2deadbe</dd>") {
		t.Error("the dead-man last-good id must be the 12-char cut, not the 6-char one")
	}
}

// TestBackupRestoreHidesAfterReveal is the nit: the reveal hides the confirm
// form and the pre-confirm drawer hides the run-host block — the hidden
// attribute must beat the display rules (.writeform, .run-host) — and the
// reveal relabels the verb `Shown` so it never reads as re-pressable.
func TestBackupRestoreHidesAfterReveal(t *testing.T) {
	h := newHarness(t)
	seedSnapshotTarget(t, h, "nas01", "a3f9c2abcdef1234567890ab", 16*time.Hour)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, "#restore-backups-form[hidden] { display:none") {
		t.Error("the revealed drawer must hide the confirm form — .writeform must not beat [hidden]")
	}
	if !strings.Contains(css, "#run-host[hidden] { display:none") {
		t.Error("the pre-confirm drawer must not render the run-host block — [hidden] must beat .run-host")
	}
	// The reveal JS relabels the verb so it never reads as re-pressable.
	_, page := getBody(t, client, ts.URL+"/system/backups")
	if !strings.Contains(page, `btn.textContent = "Shown"`) {
		t.Error("the reveal must relabel the verb 'Shown' so it does not read as re-pressable")
	}
}

// TestDeadmanPagedFactHierarchy is the nit (14): the paged fact's hierarchy —
// only `ntfy` reads loud, `once` + the age dim — via the fact's own spans,
// never one flat string.
func TestDeadmanPagedFactHierarchy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	finished := time.Now().Add(-(49*time.Hour + 2*time.Minute)).UTC()
	seedBackup(t, h, "nas01", store.BackupSucceeded, finished)
	seedHeartbeat(t, h)
	if err := h.store.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}
	if err := h.store.PutSetting(ctx, store.SettingDeadmanPagedAt,
		itoa64(time.Now().Add(-(1*time.Hour + 2*time.Minute)).UTC().UnixMilli())); err != nil {
		t.Fatalf("PutSetting paged_at: %v", err)
	}
	if err := h.store.PutSetting(ctx, store.SettingDeadmanPingedAt,
		itoa64(finished.UnixMilli())); err != nil {
		t.Fatalf("PutSetting pinged_at: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/system")
	if !strings.Contains(page, `<dd><span class="paged-word">ntfy</span><span class="paged-facts"> · once · 1h 02m ago</span></dd>`) {
		t.Error("the paged fact must split ntfy from its dim once + age")
	}
	css := cssOf(t, h, ts, client, "lift.css")
	if !strings.Contains(css, ".paged-word { color:var(--ink-100)") || !strings.Contains(css, ".paged-facts { color:var(--ink-400)") {
		t.Error("the paged fact's hierarchy must keep only ntfy loud")
	}
}

// ---- R11 · the identity strip carries the card-grid gap ----

// TestSysIdentityStripGap is the R11 gate: the strip sits flush on the card
// grid — it must carry the §0.1 grid gap as its bottom margin.
func TestSysIdentityStripGap(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "lift.css")
	start := strings.Index(css, ".sys-identity {")
	if start < 0 {
		t.Fatal("lift.css lacks the .sys-identity rule")
	}
	block := css[start:]
	end := strings.Index(block, "}")
	if end < 0 {
		t.Fatal("lift.css .sys-identity rule is unclosed")
	}
	if !strings.Contains(block[:end], "margin-bottom:20px") {
		t.Error("the identity strip must carry the 20px bottom gap onto the card grid")
	}
}

// ---- R12 · the appbar bottom reads as a real boundary ----

// TestAppbarBottomHairline is the R12 gate: the appbar's bottom border is the
// full hairline, never the faint one — the strip scrolling under it must read
// as a cut. The mutation (back to --hairline-faint) reds this test.
func TestAppbarBottomHairline(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	css := cssOf(t, h, ts, client, "app.css")
	start := strings.Index(css, ".appbar {")
	if start < 0 {
		t.Fatal("app.css lacks the .appbar rule")
	}
	block := css[start:]
	end := strings.Index(block, "}")
	if end < 0 {
		t.Fatal("app.css .appbar rule is unclosed")
	}
	block = block[:end]
	if !strings.Contains(block, "border-bottom: 1px solid var(--hairline);") {
		t.Error("the appbar bottom border must be the full hairline")
	}
	if strings.Contains(block, "var(--hairline-faint)") {
		t.Error("the appbar bottom border must not be the faint hairline")
	}
}
