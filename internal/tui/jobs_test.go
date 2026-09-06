package tui

import (
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestJobRowsRendersDegradedState pins that the jobs table shows a degraded run
// as the wait-coloured DEGRADED word rather than a bare state string.
func TestJobRowsRendersDegradedState(t *testing.T) {
	rows := jobRows([]store.Job{
		{ID: 3, Kind: "ansible", State: store.StateDegraded},
	})
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0][3] != styleWait.Render("◐ DEGRADED") {
		t.Errorf("degraded state cell = %q, want the wait DEGRADED word", rows[0][3])
	}
}

// TestJobRowsShowsSource proves the jobs table names the source a job ran
// from, dashing an empty one the same way every other unset cell renders.
func TestJobRowsShowsSource(t *testing.T) {
	rows := jobRows([]store.Job{
		{ID: 1, Kind: "ansible", State: store.StateSucceeded, Args: store.JobArgs{Source: "my-fleet"}},
		{ID: 2, Kind: "ansible", State: store.StateSucceeded},
	})
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0][1] != "my-fleet" {
		t.Errorf("row 0 source = %q, want %q", rows[0][1], "my-fleet")
	}
	if rows[1][1] != emptyCell {
		t.Errorf("row 1 (no source) = %q, want %q", rows[1][1], emptyCell)
	}
}
