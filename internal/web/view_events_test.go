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

// TestEventsSourceChip: the ?filter=checks chip narrows the page to that
// source's rows only, and the active chip carries the pressed state.
func TestEventsSourceChip(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events?filter=checks")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	if !strings.Contains(page, "check-alert") {
		t.Error("filtered page missing the matching source's row")
	}
	if strings.Contains(page, "alertmanager-alert") {
		t.Error("filtered page leaked a row from another source")
	}
	if !strings.Contains(page, `href="/events?filter=checks" aria-pressed="true"`) {
		t.Error("the checks chip is not marked active")
	}
}

// TestEventsChipCounts: the chips count the rows in view across the outcome
// and source splits, and the "all" chip sums to the tray.
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
		"all · 3", "checks · 1", "alerts · 2", "ignored · 2", "fired · 1",
		"check-alert", "alertmanager-alert", "fire-alert",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("unfiltered events page missing %q", want)
		}
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
		"nothing ran without being on this page first",
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
		fmt.Sprintf("job %d · succeeded · exit 0", job.ID),
		`href="/jobs/` + itoa(job.ID) + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("fired event missing %q", want)
		}
	}
}

// TestEventsGatedReason is the GATE for GATED enrichment: the reason names the
// approval and its TTL, and links to it.
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
		fmt.Sprintf("awaiting approval %d · TTL 30m", approval.ID),
		`href="/approvals/` + itoa(approval.ID) + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("gated event missing %q", want)
		}
	}
}

// TestEventsSuppressedReasons: the suppressed reason maps to the design copy
// for a silence and a cooldown.
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

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		"swallowed · silence pve-02 · lifts 16:00",
		"cooldown 6h · next eligible 14:22",
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
// outcome's rows.
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

	code, page := getBody(t, client, ts.URL+"/events?filter=ignored")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	if !strings.Contains(page, "chk-alert") {
		t.Error("filtered page missing the ignored row")
	}
	if strings.Contains(page, "fire-alert") {
		t.Error("filtered page leaked a non-ignored row")
	}
	if !strings.Contains(page, `href="/events?filter=ignored" aria-pressed="true"`) {
		t.Error("the ignored chip is not marked active")
	}
}

// TestEventsFilterHiddenWhenEmpty is the GATE for R7: with zero events and no
// filter the chips and tray are hidden and the W5 empty copy shows; with an
// active filter that matches nothing, the chips stay and the filtered-empty
// line reads.
func TestEventsFilterHiddenWhenEmpty(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("/events status = %d", code)
	}
	if strings.Contains(page, "filter-chip") {
		t.Error("the filter chips render with zero events (R7)")
	}
	if !strings.Contains(page, "No events yet. Point an alert source at exocomp ingest") {
		t.Error("the empty-state copy is missing")
	}
	if !strings.Contains(page, "<b>0</b><small>in</small>") {
		t.Error("the funnel does not read 0 on an empty store")
	}

	// An active filter with nothing matching keeps the chips and shows the
	// filtered-empty line (there are rows, just none gated).
	if _, err := h.store.CreateEvent(context.Background(), store.Event{
		Source: "check", Normalized: `{"name":"chk-alert"}`,
		Outcome: store.OutcomeIgnored, Reason: "no rule matched",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	code, filtered := getBody(t, client, ts.URL+"/events?filter=gated")
	if code != 200 {
		t.Fatalf("/events?filter=gated status = %d", code)
	}
	if !strings.Contains(filtered, "filter-chip") {
		t.Error("the filter chips hide even with an active filter")
	}
	if !strings.Contains(filtered, "No events match") {
		t.Error("the filtered-empty line is missing")
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
		`class="funnel-note">nothing ran without being on this page first</p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("funnel grid missing %q", want)
		}
	}
	if strings.Contains(page, "last 24h · nothing ran") {
		t.Error("funnel sentence still carries the last-24h prefix (R10)")
	}
}

// TestEventsRowLines is the GATE for R11's row restructure: each tray row is
// two .line rows — alert + outcome + chevron, then source · rule + time · rel
// ago — and the reason leaves the row entirely for the expand pane's
// "reason · …" line (SPEC §2.5), keeping the chevron and data-index cursor.
func TestEventsRowLines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Normalized: `{"name":"disk_full"}`,
		Outcome: store.OutcomeMatched, MatchedRule: "notify-on-failure",
		Reason: "log only — rule has no action",
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
		`class="evt-alert">disk_full</span>`,
		`class="evt-outcome matched"`,
		`class="evt-chev" aria-hidden="true">▸</span>`,
		"alertmanager · notify-on-failure",
		" ago",
		"reason · log only — rule has no action",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("row missing %q", want)
		}
	}
	if n := strings.Count(page, `<div class="line">`); n != 2 {
		t.Errorf("row does not render exactly two .line rows, got %d", n)
	}
	if strings.Contains(page, `class="evt-reason"`) {
		t.Error("reason leaked into the row markup (R11: pane only)")
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

// TestEventsFooter is the GATE for the footer copy (copy.txt): the enum-closed
// sentence leaves the page, leaving "showing latest {n} of {t} · retained 90d".
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

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	if !strings.Contains(page, "showing latest 3 of 3 · retained 90d") {
		t.Error("footer does not read 'showing latest 3 of 3 · retained 90d'")
	}
	for _, gone := range []string{"the enum is closed", " events · retained"} {
		if strings.Contains(page, gone) {
			t.Errorf("footer still carries the dropped copy %q", gone)
		}
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
