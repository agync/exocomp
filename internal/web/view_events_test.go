package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

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

// TestEventsSourceFilter: the ?source= filter narrows the page to that
// source's rows only, and the filter form preserves the chosen value.
func TestEventsSourceFilter(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events?source=check")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	if !strings.Contains(page, "check-alert") {
		t.Error("filtered page missing the matching source's row")
	}
	if strings.Contains(page, "alertmanager-alert") {
		t.Error("filtered page leaked a row from another source")
	}
	if !strings.Contains(page, `<option value="check" selected`) {
		t.Error("filter form does not preserve the chosen source")
	}
}

// TestEventsListAllSources: with no filter every source's rows render and the
// select defaults to all.
func TestEventsListAllSources(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		"alertmanager-alert", "check-alert",
		`<option value="" selected>all</option>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("unfiltered events page missing %q", want)
		}
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

// TestEventsJobColumn: an event that enqueued a job links to it from the
// events table's Job column.
func TestEventsJobColumn(t *testing.T) {
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
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", Normalized: `{"name":"at-home"}`, Outcome: store.OutcomeFired,
		JobID: &jobID,
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
		`<td class="num" data-label="Job">`,
		`<a href="/jobs/` + itoa(job.ID) + `">#` + itoa(job.ID) + `</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("events page missing %q", want)
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

// TestEventsLoadMore: a full page (100+) reports HasMore, so the page renders
// a Load more link carrying the next limit and preserving the source filter.
// The default floor means "more" can only be exercised by seeding past it.
func TestEventsLoadMore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < 105; i++ {
		if _, err := h.store.CreateEvent(ctx, store.Event{
			Source: "check", Normalized: `{"name":"at-home"}`,
			Outcome: store.OutcomeIgnored, Reason: "no rule matched",
		}); err != nil {
			t.Fatalf("CreateEvent %d: %v", i, err)
		}
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events?source=check")
	if code != 200 {
		t.Fatalf("events status = %d", code)
	}
	for _, want := range []string{
		`href="/events?limit=200&amp;source=check"`,
		">Load more</a>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("full events page missing %q", want)
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

// TestEventsFilterHiddenWhenEmpty is the GATE for R7: with zero events and no
// active filter the filter row is hidden (nothing to filter); with an active
// filter it stays visible and carries the filtered-empty line.
func TestEventsFilterHiddenWhenEmpty(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/events")
	if code != 200 {
		t.Fatalf("/events status = %d", code)
	}
	if strings.Contains(page, `name="source"`) {
		t.Error("the filter row renders with zero events and no filter (R7)")
	}
	if !strings.Contains(page, "No events yet. Point an alert source at exocomp ingest") {
		t.Error("the empty-state copy is missing")
	}

	// An active filter keeps the filter visible and shows the filtered-empty line.
	code, filtered := getBody(t, client, ts.URL+"/events?source=check")
	if code != 200 {
		t.Fatalf("/events?source=check status = %d", code)
	}
	if !strings.Contains(filtered, `name="source"`) {
		t.Error("the filter row hides even with an active filter (R7)")
	}
	if !strings.Contains(filtered, "No events from") {
		t.Error("the filtered-empty line is missing")
	}
}
