package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// phone_r2_read_test.go — the phone-web round-2 read-surface gates: R1
// (dashboard insecure banner), R2 (queue idle kicker + row name), R8 (events
// outcome chips, the rule-unset row marker, the count row), R9 (secrets card).
// Layout is browser-verified at 390px; these pin the markup and the CSS rules
// the phone re-flow consumes.

// TestPhoneDashboardBannerStacks is the R1 gate: at 390px the insecure banner
// is two flex columns and the sentence wraps one word per line with its last
// line clipped. The phone rule stacks it — the kicker on line 1, the sentence
// below, overflow visible and no fixed height. The MUTATION — dropping the
// phone rule — reds this test.
func TestPhoneDashboardBannerStacks(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	for _, want := range []string{
		".banner { flex-direction: column; align-items: flex-start; gap: 6px; height: auto; overflow: visible; padding: 12px 14px; }",
		".banner .keyword { font: 600 11px var(--font-mono); }",
		// The TLS verb carries the global .verb glyph-button shape (34px square)
		// and its text wraps one word per line inside it — the real cause of the
		// banner's wrap. In the banner it is a plain text link.
		".banner .verb { display: inline; width: auto; height: auto; color: var(--accent-text); }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the R1 phone banner rule %q", want)
		}
	}

	var buf bytes.Buffer
	if err := dashboard(dashboardVM{Page: page{Insecure: true}}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render dashboard: %v", err)
	}
	if !strings.Contains(buf.String(), `<div class="banner fail" role="alert">`) {
		t.Error("the insecure banner must keep the real .banner.fail classes")
	}
}

// TestPhoneQueueIdleKickerRealigned is the R2 gate: the idle kicker's phone
// padding rule is defeated by the later base `.queue-idle` rule at equal
// specificity, so the phone inset must be re-asserted AFTER the base block, and
// the recent row's name cell must be ink rather than the a{accent-text} purple.
// The MUTATION — removing the re-assertion or the ink colour — reds this test.
func TestPhoneQueueIdleKickerRealigned(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	base := strings.Index(app, ".queue-idle { display: flex; align-items: center; gap: 14px; height: var(--row-h); padding: 0 14px;")
	phone := strings.LastIndex(app, ".queue-idle { height: auto; min-height: var(--row-h); padding: 8px 12px;")
	if base < 0 || phone < base {
		t.Error("app.css must re-assert the .queue-idle phone padding after the base rule")
	}
	if !strings.Contains(app, ".queue-row .cell { display: flex; flex-direction: column; gap: 4px; min-width: 0; color: var(--ink-100); }") {
		t.Error("the queue row name cell must be ink, not the a{accent-text} purple")
	}
	// The LAST RUNS rule's base 14px inset sits 2px right of the rows' 12px
	// inset; the phone rule carries the same 12px so kicker, rows and rule
	// share one left edge.
	if !strings.Contains(app, ".queue-rule { display: flex; justify-content: space-between; padding: 0 12px; }") {
		t.Error("the queue rule's phone inset must match the rows and the idle kicker")
	}
}

// TestPhoneEventsOutcomeChips pins the R8 outcome-chip rule on phone: 32px,
// 12px mono. The mutation — dropping the phone rule — lets the 36px
// `.chips .chip` rule win and reds this test.
func TestPhoneEventsOutcomeChips(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, ".filters .chips .chip { height: 32px; padding: 0 12px; border-radius: var(--radius-chip); font: 400 12px var(--font-mono); }") {
		t.Error("app.css missing the R8 outcome-chip phone rule (32px / 12px mono)")
	}
}

// TestPhoneEventsRuleUnsetMarker is the R8 gate for the unset rule: an event
// with no matched rule renders `source → -` on the phone line 2 because orDash
// prints "-". The row carries a new rule-unset marker and the phone stylesheet
// hides the rule cell (and its ` → ` separator) — never a second template. The
// MUTATION — dropping the marker — reds this test.
func TestPhoneEventsRuleUnsetMarker(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"no-rule"}`,
		Outcome: store.OutcomeIgnored, Reason: "no rule matched",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != http.StatusOK {
		t.Fatalf("events status = %d, want 200", code)
	}
	if !strings.Contains(page, `class="events-row evt-rule-unset" role="button"`) {
		t.Error("a row with no matched rule must carry the evt-rule-unset marker")
	}
	if !strings.Contains(page, `<span class="evt-rule">-</span>`) {
		t.Error("the desktop rule cell keeps the orDash placeholder")
	}

	app := cssOf(t, h, ts, client, "app.css")
	if !strings.Contains(app, ".events-row.evt-rule-unset .evt-rule { display: none; }") {
		t.Error("app.css must hide the phone rule cell (and its arrow) when the rule is unset")
	}
}

// TestPhoneEventsCountRowFits is the R8 gate for the funnel counts: the six
// `n label` pairs are wider than 390px, so on phone they flow in 12px mono and
// wrap to fit the card with no horizontal scroll (a list, not a chart). The
// proportional bar is hidden. The MUTATION — nowrap/grid or the bar — reds this.
func TestPhoneEventsCountRowFits(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	app := cssOf(t, h, ts, client, "app.css")
	for _, want := range []string{
		".funnel .counts { display: flex; flex-wrap: wrap; align-items: baseline; gap: 8px 14px; }",
		".funnel .counts b { font: 500 12px/1 var(--font-mono); display: inline; }",
		".funnel .counts small { font: 400 12px var(--font-mono); }",
		".funnel .bar { display: none; }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the R8 phone count-row rule %q", want)
		}
	}
}

// TestPhoneSecretsCardDropsRedundantHeading is the R9 gate: the card's kicker,
// the "All secrets" h3 and the empty-state sentence all said one thing. The
// redundant middle line is gone; the kicker and the empty-state sentence stay.
// The MUTATION — restoring the h3 — reds this test.
func TestPhoneSecretsCardDropsRedundantHeading(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/secrets")
	if code != http.StatusOK {
		t.Fatalf("/secrets status = %d, want 200", code)
	}
	if strings.Contains(page, "<h3>All secrets</h3>") {
		t.Error("the secrets card's redundant 'All secrets' heading must be gone")
	}
	if !strings.Contains(page, `<div class="kicker">Sealed values</div>`) {
		t.Error("the secrets card must keep its kicker")
	}
	if !strings.Contains(page, "No secrets sealed.") {
		t.Error("the secrets card must keep its empty-state sentence")
	}
}
