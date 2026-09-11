package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// seedEvents writes one ignored event per source so the filter tests have rows
// from distinct pipelines to separate.
func seedEvents(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	for _, src := range []string{"alertmanager", "check"} {
		if _, err := h.store.CreateEvent(ctx, store.Event{
			Source: src, Normalized: `{"name":"` + src + `-alert"}`,
			Outcome: store.OutcomeIgnored, Reason: "no rule matched",
		}); err != nil {
			t.Fatalf("CreateEvent %s: %v", src, err)
		}
	}
}

// TestEventsSourceChip: the source switch narrows the page to that source's
// rows only, and exactly one segment carries the pressed state (all = reset).
// The store's singular ?source=check and the legacy ?filter=checks alias both
// resolve onto the same split.
func TestEventsSourceChip(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events?source=checks")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	if !strings.Contains(page, "check-alert") {
		t.Error("filtered page missing the matching source's row")
	}
	if strings.Contains(page, "alertmanager-alert") {
		t.Error("filtered page leaked a row from another source")
	}
	if !strings.Contains(page, `href="/events?source=checks" aria-pressed="true"`) {
		t.Error("the checks segment is not marked pressed")
	}
	// Exactly one segment is on: all is unpressed when a split is active.
	if !strings.Contains(page, `href="/events" aria-pressed="false"`) {
		t.Error("the all segment must unpin when another split is active")
	}
	// Unfiltered, the all segment is the one pressed (the reset).
	_, unfiltered := getBody(t, client, ts.URL+"/events")
	if !strings.Contains(unfiltered, `href="/events" aria-pressed="true"`) {
		t.Error("the all segment must be pressed only when no source is active")
	}

	// Singular alias: the store vocabulary's ?source=check links keep working.
	_, singular := getBody(t, client, ts.URL+"/events?source=check")
	if !strings.Contains(singular, "check-alert") || strings.Contains(singular, "alertmanager-alert") {
		t.Error("the singular ?source=check alias must filter like checks")
	}
	if !strings.Contains(singular, `href="/events?source=checks" aria-pressed="true"`) {
		t.Error("the singular alias must still mark the checks segment pressed")
	}

	// Legacy alias: ?filter=checks keeps working.
	_, legacy := getBody(t, client, ts.URL+"/events?filter=checks")
	if !strings.Contains(legacy, "check-alert") || strings.Contains(legacy, "alertmanager-alert") {
		t.Error("the legacy ?filter=checks alias must filter checks")
	}
	if !strings.Contains(legacy, `href="/events?source=checks" aria-pressed="true"`) {
		t.Error("the legacy alias must still mark the checks segment pressed")
	}
}

// TestEventsChipCounts: the chips count the rows in view across the outcome
// and source splits, and the "all" segment sums to the tray. Every count is
// its own tabular cell rendered as a full/compact pair, and all six outcome
// chips render in funnel order.
func TestEventsChipCounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedEvents(t, h)
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"fire-alert"}`,
		Outcome: store.OutcomeFired, Reason: "job 1 queued",
	}); err != nil {
		t.Fatalf("CreateEvent fired: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		`<span class="count"><span class="full">3</span><span class="k">3</span></span>`,
		`<span class="count"><span class="full">2</span><span class="k">2</span></span>`,
		`<span class="count"><span class="full">1</span><span class="k">1</span></span>`,
		"check-alert", "alertmanager-alert", "fire-alert",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("unfiltered events page missing %q", want)
		}
	}
	// The groups are named by aria-label + title, never a visible kicker.
	for _, want := range []string{
		`role="group" aria-label="Event source"`,
		`role="group" aria-label="Event outcome"`,
		`title="Source — where the event came from"`,
		`<span class="divider" aria-hidden="true">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("filter row missing %q", want)
		}
	}
	// All six outcome chips render in funnel order, zero counts as disabled
	// spans (matched/suppressed/gated/error are zero in this fixture).
	funnelOrder := []string{"ignored", "matched", "suppressed", "gated", "fired", "error"}
	prev := -1
	for _, o := range funnelOrder {
		i := strings.Index(page, ">"+o+"<")
		if i < 0 {
			t.Errorf("outcome chip %q is missing from the row", o)
			continue
		}
		if i < prev {
			t.Errorf("outcome chips must render in funnel order, %q out of place", o)
		}
		prev = i
	}
	if !strings.Contains(page, `<span class="chip" aria-disabled="true">matched`) {
		t.Error("a zero-count outcome must render as a disabled span, not a link")
	}
}

// TestCountFormatting pins the count cell helpers: thousands separators on the
// full twin ("1,204") and the compact k-form ("1.2k", "12.4k").
func TestCountFormatting(t *testing.T) {
	if got := commaCount(999); got != "999" {
		t.Errorf("commaCount(999) = %q, want 999", got)
	}
	if got := commaCount(1204); got != "1,204" {
		t.Errorf("commaCount(1204) = %q, want 1,204", got)
	}
	if got := commaCount(12040); got != "12,040" {
		t.Errorf("commaCount(12040) = %q, want 12,040", got)
	}
	if got := compactCount(1204); got != "1.2k" {
		t.Errorf("compactCount(1204) = %q, want 1.2k", got)
	}
	if got := compactCount(12400); got != "12.4k" {
		t.Errorf("compactCount(12400) = %q, want 12.4k", got)
	}
	if got := compactCount(999); got != "999" {
		t.Errorf("compactCount(999) = %q, want 999", got)
	}
}

// TestEventsFunnel is the GATE for the web funnel card: events received in the
// last 24h are tallied by outcome and rendered, and an event older than the
// window is excluded from the counts (though it still lists in the tray).
func TestEventsFunnel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := h.store.CreateEvent(ctx, store.Event{
			Source: "alertmanager", Normalized: `{"name":"ignored-alert"}`,
			Outcome: store.OutcomeIgnored, Reason: "no rule matched",
			ReceivedAt: now.Add(-time.Minute),
		}); err != nil {
			t.Fatalf("CreateEvent ignored: %v", err)
		}
	}
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"matched-alert"}`,
		Outcome: store.OutcomeMatched, Reason: "notify only; no job",
		ReceivedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateEvent matched: %v", err)
	}
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"fire-alert"}`,
		Outcome: store.OutcomeFired, Reason: "job 1 queued",
		ReceivedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateEvent fired: %v", err)
	}
	// Older than the window: in the tray, out of the funnel.
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"stale-alert"}`,
		Outcome: store.OutcomeFired, Reason: "job 2 queued",
		ReceivedAt: now.Add(-25 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateEvent stale: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		"<b>4</b><small>in</small>", "<b>2</b><small>ignored</small>",
		"<b>1</b><small>matched</small>", "<b>1</b><small>fired</small>",
		`aria-label="Alert funnel, last 24 hours"`,
		"stale-alert",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("funnel missing %q", want)
		}
	}
}

// TestEventsFiredReason is the GATE for FIRED enrichment: the reason column
// overrides the emission-time "job N queued" sentence with the live job facts
// and links to the job.
func TestEventsFiredReason(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "deadman-restart", Kind: "shell", Trigger: "rule",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job = h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)
	jobID := job.ID
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"deadman_ping_missing"}`,
		Outcome: store.OutcomeFired, Reason: fmt.Sprintf("job %d queued", jobID),
		JobID: &jobID,
	}); err != nil {
		t.Fatalf("CreateEvent fired: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		fmt.Sprintf("job #%d · succeeded · exit 0", job.ID),
		`href="/jobs/` + itoa(job.ID) + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("fired event missing %q", want)
		}
	}
}

// TestEventsGatedReason is the GATE for GATED enrichment: the reason reads
// "awaiting approval" and links to the approval.
func TestEventsGatedReason(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	job, approval := h.gatedJob(t, 30*time.Minute)
	jobID := job.ID
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"dns_records_drifted"}`,
		Outcome: store.OutcomeGated, Reason: fmt.Sprintf("approval %d pending", approval.ID),
		JobID: &jobID,
	}); err != nil {
		t.Fatalf("CreateEvent gated: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		"awaiting approval",
		`href="/approvals/` + itoa(approval.ID) + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("gated event missing %q", want)
		}
	}
}

// TestEventsSuppressedReasons: the suppressed reason maps to the design copy
// for a silence, a cooldown ("cooldown {d} left") and a duplicate ("duplicate
// of #{id}").
func TestEventsSuppressedReasons(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"zfs_pool_degraded"}`,
		Outcome: store.OutcomeSuppressed,
		Reason:  "silenced: host pve-02 until 16:00 (3 swallowed)",
	}); err != nil {
		t.Fatalf("CreateEvent silence: %v", err)
	}
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"restic_lock_held"}`,
		Outcome: store.OutcomeSuppressed,
		Reason:  "cooldown 6h, next eligible 14:22",
	}); err != nil {
		t.Fatalf("CreateEvent cooldown: %v", err)
	}
	anchor, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"node_disk_82pct"}`,
		Outcome: store.OutcomeFired, Reason: "job 1 queued",
	})
	if err != nil {
		t.Fatalf("CreateEvent anchor: %v", err)
	}
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"node_disk_82pct"}`,
		Outcome: store.OutcomeSuppressed,
		Reason:  fmt.Sprintf("duplicate of event %d within 5m", anchor.ID),
	}); err != nil {
		t.Fatalf("CreateEvent duplicate: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		"swallowed · silence pve-02 · lifts 16:00",
		"cooldown 6h left",
		"duplicate of #" + itoa(anchor.ID),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("suppressed event missing %q", want)
		}
	}
}

// TestEventsExpandPane is the GATE for the two-pane expand: the row's inline
// pane carries the pretty-printed normalized payload and the evaluation trace
// steps, both read from the stored row.
func TestEventsExpandPane(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"dns_records_drifted","severity":"warning"}`,
		Outcome: store.OutcomeGated, Reason: "approval 1 pending",
		Trace: `[{"step":"rule","detail":"drift-replan","passed":true,"reason":"matched drift-replan"},` +
			`{"step":"silence","detail":"","passed":true,"reason":"no silence covers this event"}]`,
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		"NORMALIZED PAYLOAD",
		// The pre renders the stored JSON HTML-escaped, key order intact.
		`&#34;name&#34;: &#34;dns_records_drifted&#34;`,
		"EXPLAIN · RULE EVALUATION TRACE",
		"drift-replan",
		"matched drift-replan",
		"PASS",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("expand pane missing %q", want)
		}
	}
}

// TestEventsOutcomeChipFilter: the outcome chip narrows the tray to that
// outcome's rows. The active chip is the only accent on the row and carries
// the trailing × clear: its href is the reset (the outcome param removed) and
// its title reads "Clear filter". The legacy ?filter=fired alias still works.
func TestEventsOutcomeChipFilter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", Normalized: `{"name":"chk-alert"}`,
		Outcome: store.OutcomeIgnored, Reason: "no rule matched",
	}); err != nil {
		t.Fatalf("CreateEvent ignored: %v", err)
	}
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"fire-alert"}`,
		Outcome: store.OutcomeFired, Reason: "job 1 queued",
	}); err != nil {
		t.Fatalf("CreateEvent fired: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events?outcome=fired")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	if !strings.Contains(page, "fire-alert") {
		t.Error("filtered page missing the fired row")
	}
	if strings.Contains(page, "chk-alert") {
		t.Error("filtered page leaked a non-fired row")
	}
	if !strings.Contains(page, `<a class="chip" href="/events" aria-pressed="true" title="Clear filter">fired`) {
		t.Error("the active chip must be the accent, its href the reset, and carry the Clear filter title")
	}
	// The trailing × glyph rides inside the active chip.
	if !strings.Contains(page, `<svg class="clear" width="14" height="14"`) {
		t.Error("the active chip must carry the trailing × clear glyph")
	}
	// The all segment is pressed (no source filter) and the fired chip pressed.
	if !strings.Contains(page, `href="/events" aria-pressed="true"`) {
		t.Error("the all segment must be pressed alongside an outcome filter")
	}

	// Legacy alias: ?filter=fired keeps working.
	_, legacy := getBody(t, client, ts.URL+"/events?filter=fired")
	if !strings.Contains(legacy, "fire-alert") || strings.Contains(legacy, "chk-alert") {
		t.Error("the legacy ?filter=fired alias must filter fired")
	}

	// Composing: the source switch preserves the active outcome (the href
	// escapes the & of the composed query).
	_, composed := getBody(t, client, ts.URL+"/events?source=checks&outcome=fired")
	if !strings.Contains(composed, `href="/events?source=checks&amp;outcome=fired" aria-pressed="true"`) {
		t.Error("the source segments must preserve the composed outcome param")
	}
}

// TestEventsFilterHiddenWhenEmpty: the closed enum supersedes the old
// empty-hide — the source switch and all six outcome chips render even with
// zero events (zero counts as disabled spans), while the tray gives way to the
// W5 empty copy. The old "No events match" branch is gone.
func TestEventsFilterHiddenWhenEmpty(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("/events status = %d", code)
	}
	if !strings.Contains(page, `class="switch"`) {
		t.Error("the source switch must render even with zero events (closed enum)")
	}
	if !strings.Contains(page, `class="group group--outcome chips"`) {
		t.Error("the outcome chips must render even with zero events (closed enum)")
	}
	if !strings.Contains(page, `aria-disabled="true"`) {
		t.Error("the zero-count outcome chips must render as disabled spans")
	}
	if !strings.Contains(page, "No events yet. Point an alert source at exocomp ingest") {
		t.Error("the empty-state copy is missing")
	}
	if !strings.Contains(page, "<b>0</b><small>in</small>") {
		t.Error("the funnel does not read 0 on an empty store")
	}
	if strings.Contains(page, "No events match") {
		t.Error("the 'No events match' branch must be removed")
	}
}

// TestEventName: the shared wire.EventName the event rows render through — a
// check transition body carries no name, so the alert reads as check · target;
// a body that does carry a name keeps it, and an unnamed body falls back to the
// source.
func TestEventName(t *testing.T) {
	cases := []struct {
		name string
		ev   store.Event
		want string
	}{
		{"check transition", store.Event{
			Source:     reactive.SourceCheck,
			Normalized: `{"check":"at-home","target":"laptop","kind":"tcp","address":"192.168.1.50:22"}`,
		}, "at-home · laptop"},
		{"check body with name wins", store.Event{
			Source:     reactive.SourceCheck,
			Normalized: `{"name":"at-home"}`,
		}, "at-home"},
		{"alertmanager keeps its name", store.Event{
			Source:     reactive.SourceAlertmanager,
			Normalized: `{"name":"DiskFull","severity":"critical"}`,
		}, "DiskFull"},
		{"unnamed falls back to source", store.Event{
			Source:     reactive.SourceGeneric,
			Normalized: `{"severity":"critical"}`,
		}, reactive.SourceGeneric},
	}
	for _, tc := range cases {
		if got := wire.EventName(tc.ev); got != tc.want {
			t.Errorf("%s: EventName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestEventsTraceJobLink: the trace page names the job an event enqueued and
// links to it.
func TestEventsTraceJobLink(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "rule:gate-ssh", Kind: "playbook", Trigger: "rule",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	jobID := job.ID
	ev, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", Normalized: `{"name":"at-home"}`, Outcome: store.OutcomeFired,
		JobID: &jobID,
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events/"+itoa(ev.ID))
	if code != 200 {
		t.Fatalf("event trace status = %d", code)
	}
	for _, want := range []string{
		"enqueued job",
		`<a href="/jobs/` + itoa(job.ID) + `">#` + itoa(job.ID) + `</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("event trace missing %q", want)
		}
	}
}

// TestEventTraceBreadcrumb:the event trace view carries a breadcrumb trail back
// to the events list, ending in the current event (unlinked, aria-current).
func TestEventTraceBreadcrumb(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ev, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", Normalized: `{"name":"at-home"}`, Outcome: store.OutcomeIgnored,
		Reason: "no rule matched",
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events/"+itoa(ev.ID))
	if code != 200 {
		t.Fatalf("event trace status = %d", code)
	}
	for _, want := range []string{
		`class="breadcrumb"`,
		`href="/events">Events</a>`,
		`aria-current="page">#` + itoa(ev.ID) + `</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("event trace missing breadcrumb %q", want)
		}
	}
}

// TestEventTraceCheckLedger is the GATE for the hysteresis ledger card on a
// check event's Explain page: the stored observations render as the
// "observed {status} {hh:mm:ss} · {i} of 3" lines with the transition marker,
// the hysteresis note, and the cold-start body; an alert event renders none of
// the card.
func TestEventTraceCheckLedger(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	checkEv, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", Normalized: `{"name":"at-home","check":"at-home","target":"laptop"}`,
		Outcome: store.OutcomeFired, Reason: "transition",
		CheckObservations: `[{"observed_at":"2026-09-01T13:41:15Z","status":"up","consecutive":1},` +
			`{"observed_at":"2026-09-01T13:41:30Z","status":"up","consecutive":2},` +
			`{"observed_at":"2026-09-01T13:41:45Z","status":"up","consecutive":3}]`,
	})
	if err != nil {
		t.Fatalf("CreateEvent check: %v", err)
	}
	alertEv, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"DiskFull"}`, Outcome: store.OutcomeIgnored,
		Reason: "no rule matched",
	})
	if err != nil {
		t.Fatalf("CreateEvent alert: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events/"+itoa(checkEv.ID))
	if code != 200 {
		t.Fatalf("check trace status = %d", code)
	}
	for _, want := range []string{
		`Why this counted as a transition`,
		`observed up   13:41:15   1 of 3`,
		`observed up   13:41:45   3 of 3 →`,
		`hysteresis N=3 · one blip is not a transition`,
		`absence of history is cold-start, not health`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("check trace missing ledger marker %q", want)
		}
	}

	code, alert := getBody(t, client, ts.URL+"/events/"+itoa(alertEv.ID))
	if code != 200 {
		t.Fatalf("alert trace status = %d", code)
	}
	if strings.Contains(alert, "Why this counted as a transition") {
		t.Error("an alert event must not render the hysteresis ledger card")
	}
}

// TestEventsFunnelGrid is the GATE for R10: the funnel renders as a three-column
// count grid (SPEC §2.5) — a monospace numeral over a small label per closed
// outcome, each cell wearing its enum colour class, a proportional bar, and the
// verbatim sentence with the "last 24h ·" prefix gone (that fact moved to the
// app bar's meta slot).
func TestEventsFunnelGrid(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := h.store.CreateEvent(ctx, store.Event{
			Source: "alertmanager", Normalized: `{"name":"ignored-alert"}`,
			Outcome: store.OutcomeIgnored, Reason: "no rule matched",
		}); err != nil {
			t.Fatalf("CreateEvent ignored: %v", err)
		}
	}
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"matched-alert"}`,
		Outcome: store.OutcomeMatched, Reason: "notify only",
	}); err != nil {
		t.Fatalf("CreateEvent matched: %v", err)
	}
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"fire-alert"}`,
		Outcome: store.OutcomeFired, Reason: "job 1 queued",
	}); err != nil {
		t.Fatalf("CreateEvent fired: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		`<div class="counts funnel-counts">`,
		"<b>4</b><small>in</small>", "<b>2</b><small>ignored</small>",
		"<b>1</b><small>matched</small>", "<b>0</b><small>suppressed</small>",
		"<b>0</b><small>gated</small>", "<b>1</b><small>fired</small>",
		`class="funnel-ct funnel-in"`, `class="funnel-ct"`,
		`class="funnel-ct matched"`, `class="funnel-ct wait"`,
		`class="funnel-ct fired on"`,
		`class="bar funnel-bar"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("funnel grid missing %q", want)
		}
	}
	if strings.Contains(page, "last 24h · nothing ran") {
		t.Error("funnel sentence still carries the last-24h prefix (R10)")
	}
}

// TestEventsTrayGrid is the GATE for the tray's 7-column single-line desktop
// grid (SPEC §1a, SCREENS 1a): RECEIVED · EVENT · SOURCE · MATCHED RULE ·
// OUTCOME · REASON · ▸ in one 44px row. The header carries the six labels,
// the RECEIVED cell keeps "HH:MM:SS · Nm ago" in ONE cell, the reason renders
// in the row with its link, and the phone re-flows the same DOM — never a
// second template.
func TestEventsTrayGrid(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"disk_full"}`,
		Outcome: store.OutcomeMatched, MatchedRule: "notify-on-failure",
		Reason: "notify only; no job",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		`class="events-row" role="button" tabindex="0" data-index="0"`,
		// The header names the six labelled columns; the chevron slot is bare.
		`<span>RECEIVED</span><span>EVENT</span><span>SOURCE</span><span>MATCHED RULE</span><span>OUTCOME</span><span>REASON</span>`,
		// The seven cells in grid order: received · alert · source · rule ·
		// outcome · reason · chevron.
		`<span class="evt-received"`,
		`<span class="evt-alert">disk_full</span>`,
		`<span class="evt-source">alertmanager</span>`,
		`<span class="evt-rule">notify-on-failure</span>`,
		`class="evt-outcome matched"`,
		`<span class="evt-chev" aria-hidden="true">▸</span>`,
		// The reason renders in the row (the "why" column), matched copy with
		// the rule name.
		`<span class="evt-reason">rule notify-on-failure · not yet run</span>`,
		// The RECEIVED cell keeps clock · age in one cell.
		" ago",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("tray grid missing %q", want)
		}
	}
	// The RECEIVED cell is one cell: "HH:MM:SS · Nm ago" in a single span.
	rec := strings.Index(page, `<span class="evt-received"`)
	if rec < 0 {
		t.Fatal("tray row received cell missing")
	}
	recEnd := strings.Index(page[rec:], "</span>")
	one := page[rec : rec+recEnd+len("</span>")]
	if strings.Count(one, "<span") != 1 {
		t.Error("the RECEIVED cell must be a single span, not split across cells")
	}
	if !strings.Contains(one, " · ") || !strings.Contains(one, " ago") {
		t.Error("the RECEIVED cell must read '{clock} · {rel} ago' in one cell")
	}
	// The reason stays answerable in the expand pane too (the phone re-flow
	// drops the column, not the why).
	if !strings.Contains(page, "reason · rule notify-on-failure · not yet run") {
		t.Error("the expand pane lost its reason line")
	}
	// The old two-.line wrappers are gone: one structure, re-flowed.
	if strings.Contains(page, `<div class="line">`) {
		t.Error("the tray row must not carry the old .line wrappers")
	}
	// The desktop grid and the phone re-flow are CSS, not a second template.
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	if !strings.Contains(app, "grid-template-columns: 152px minmax(0,1.1fr) 84px 150px 96px minmax(0,1.5fr) 20px") {
		t.Error("app.css missing the 7-column tray grid")
	}
}

// TestEventsMatchedReason pins the matched REASON copy (SPEC §1a): a matched
// event reads "rule {name} · not yet run", the rule name from the event row —
// not the stored emission-time sentence.
func TestEventsMatchedReason(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", Normalized: `{"name":"at-home"}`,
		Outcome: store.OutcomeMatched, MatchedRule: "onboard-node0",
		Reason: "notify only; no job",
	}); err != nil {
		t.Fatalf("CreateEvent matched: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	if !strings.Contains(page, "rule onboard-node0 · not yet run") {
		t.Error("matched reason must read 'rule {name} · not yet run'")
	}
	if strings.Contains(page, "log only — rule has no action") {
		t.Error("the old matched sentence must be gone")
	}
}

// TestEventsPayloadAfterTrace pins the phone expand-pane order (SPEC §2.5):
// reason and trace first, then the "Show normalized payload →" button, then the
// payload card — so the payload follows the trace in the DOM.
func TestEventsPayloadAfterTrace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"dns_records_drifted"}`,
		Outcome: store.OutcomeGated, Reason: "approval 1 pending",
		Trace: `[{"step":"rule","detail":"drift-replan","passed":true,"reason":"matched"}]`,
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	explain := strings.Index(page, "EXPLAIN · RULE EVALUATION TRACE")
	toggle := strings.Index(page, "Show normalized payload →")
	payload := strings.Index(page, "NORMALIZED PAYLOAD")
	if explain < 0 || toggle < 0 || payload < 0 {
		t.Fatalf("pane markers missing (explain=%d toggle=%d payload=%d)", explain, toggle, payload)
	}
	if explain >= toggle || toggle >= payload {
		t.Errorf("pane order wrong: trace %d, toggle %d, payload %d (want trace < toggle < payload)", explain, toggle, payload)
	}
	if !strings.Contains(page, `class="payload"`) {
		t.Error("the payload card does not carry the .payload class")
	}
}

// TestEventsFooter is the GATE for the footer copy (copy.txt): with no outcome
// filter it reads "showing latest {n} of {total} · retained 90d"; with one,
// "showing latest {n} of {count} {outcome} · {total} in view · retained 90d ·
// show all", where {count} is the outcome's last-24h count and {total} the
// funnel total.
func TestEventsFooter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := h.store.CreateEvent(ctx, store.Event{
			Source: "alertmanager", Normalized: `{"name":"alert"}`,
			Outcome: store.OutcomeIgnored, Reason: "no rule matched",
		}); err != nil {
			t.Fatalf("CreateEvent: %v", err)
		}
	}
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"fire-alert"}`,
		Outcome: store.OutcomeFired, Reason: "job 1 queued",
	}); err != nil {
		t.Fatalf("CreateEvent fired: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	if !strings.Contains(page, "showing latest 4 of 4 · retained 90d") {
		t.Error("footer without an outcome filter must read 'showing latest 4 of 4 · retained 90d'")
	}
	if strings.Contains(page, "show all") {
		t.Error("footer without an outcome filter has nothing to show all")
	}

	code, fired := getBody(t, client, ts.URL+"/events?outcome=fired")
	if code != 200 {
		t.Fatalf("events?outcome=fired status = %d", code)
	}
	if !strings.Contains(fired, `showing latest 1 of 1 fired · 4 in view · retained 90d · <a href="/events">show all</a>`) {
		t.Error("footer with an outcome filter must read 'showing latest 1 of 1 fired · 4 in view · retained 90d · show all'")
	}
}

// TestEventsKeysHintInFunnel pins the keys hint move (§1.6): the hint leaves
// the filter row for the funnel card's meta line, gains "o outcome", and the
// keydown handler cycles the outcome group with o.
func TestEventsKeysHintInFunnel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", Normalized: `{"name":"at-home"}`,
		Outcome: store.OutcomeIgnored, Reason: "no rule matched",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/events")
	funnel := strings.Index(page, `class="card funnel"`)
	keys := strings.Index(page, `<span class="keys" aria-hidden="true">`)
	if funnel < 0 || keys < funnel {
		t.Error("the keys hint must live inside the funnel card")
	}
	for _, want := range []string{
		`j</kbd> <kbd>k</kbd> move · <kbd>↵</kbd> expand · <kbd>e</kbd> explain · <kbd>o</kbd> outcome`,
		`data-source="" data-outcome=""`, // the filters row names the active state for the o key
		`} else if (e.key === "o") {`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("keys hint missing %q", want)
		}
	}
	if strings.Contains(page, `class="keys-hint"`) {
		t.Error("the old .keys-hint markup must leave the filter row")
	}
}

// TestEventsAppbarMetaLast24h is the GATE for the app-bar meta slot: it reads
// "last 24h" (the funnel's window, moved out of the funnel note per R10).
func TestEventsAppbarMetaLast24h(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", Normalized: `{"name":"at-home"}`,
		Outcome: store.OutcomeIgnored, Reason: "no rule matched",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	if !strings.Contains(page, `<span class="appbar-meta">last 24h</span>`) {
		t.Error("app bar meta does not read 'last 24h'")
	}
	if strings.Contains(page, "every alert received") {
		t.Error("old app bar meta string still present")
	}
}

// TestKeysHintCoarseCSS is the GATE for R12: the keys hint markup stays but the
// stylesheet hides it on touch via (pointer:coarse) — asserted on the CSS, not
// the rendered page, since the guard is a media query (Shell ships it).
func TestKeysHintCoarseCSS(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	if !strings.Contains(app, "@media (pointer: coarse)") {
		t.Error("app.css missing the (pointer: coarse) touch-hint rule")
	}
	if !strings.Contains(app, "@media (pointer: coarse) { .keys-hint { display: none; } }") {
		t.Error("the coarse-pointer guard does not hide .keys-hint")
	}
}
