package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
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
