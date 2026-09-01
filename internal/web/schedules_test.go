package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestScheduleRowsHaveDetailsButton: every schedule row carries a focusable
// Details button that opens the same per-row spec modal the row's mouse click
// opens — so a keyboard operator can reach the spec without a pointer.
func TestScheduleRowsHaveDetailsButton(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly-drift", CronExpr: "@every 6h", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"echo", "hi"}},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("GET /schedules = %d, want 200", code)
	}
	for _, want := range []string{
		`data-modal-open="sched-nightly-drift"`,
		`>Details</button>`,
		`id="sched-nightly-drift"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/schedules missing %q", want)
		}
	}
}
