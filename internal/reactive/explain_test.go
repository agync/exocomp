package reactive_test

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// TestRenderCheckLedger pins the hysteresis ledger `event explain` renders for
// a check transition: one observation per line in the shipped shape, the last
// one carrying the transition marker, then the hysteresis note and the
// cold-start body. The ledger is embedded on the event row at emission; a
// render is a pure read of it, never a recompute.
func TestRenderCheckLedger(t *testing.T) {
	t.Parallel()
	ledger := `[{"observed_at":"2026-09-01T13:41:15Z","status":"up","consecutive":1},` +
		`{"observed_at":"2026-09-01T13:41:30Z","status":"up","consecutive":2},` +
		`{"observed_at":"2026-09-01T13:41:45Z","status":"up","consecutive":3}]`
	x := reactive.Explanation{
		Event: store.Event{
			ID: 1, Source: reactive.SourceCheck, Outcome: store.OutcomeIgnored,
			Reason: "no rule", CheckObservations: ledger,
		},
	}
	rendered := x.Render()
	for _, want := range []string{
		"ledger:",
		"observed up 13:41:15 · 1 of 3",
		"observed up 13:41:30 · 2 of 3",
		"observed up 13:41:45 · 3 of 3 → transition",
		"hysteresis N=3 · one blip is not a transition",
		"Absence of history is cold-start, not health.",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("check explain does not render %q:\n%s", want, rendered)
		}
	}
}

// TestRenderSkipsLedgerForAlertEvent proves the ledger block is additive: an
// alert event, or a check event without observations, renders exactly the
// pre-ledger output and nothing extra.
func TestRenderSkipsLedgerForAlertEvent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		event store.Event
	}{
		{"alert", store.Event{ID: 2, Source: reactive.SourceAlertmanager, Outcome: store.OutcomeFired, Reason: "rule matched"}},
		{"check without observations", store.Event{ID: 3, Source: reactive.SourceCheck, Outcome: store.OutcomeIgnored, Reason: "no rule"}},
		{"check with empty ledger", store.Event{ID: 4, Source: reactive.SourceCheck, Outcome: store.OutcomeIgnored, Reason: "no rule", CheckObservations: "[]"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			x := reactive.Explanation{Event: tc.event}
			rendered := x.Render()
			if strings.Contains(rendered, "ledger:") {
				t.Fatalf("explain rendered a ledger for %s:\n%s", tc.name, rendered)
			}
			if strings.Contains(rendered, "hysteresis") {
				t.Fatalf("explain rendered the hysteresis note for %s:\n%s", tc.name, rendered)
			}
		})
	}
}
