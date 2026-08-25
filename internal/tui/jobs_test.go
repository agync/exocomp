package tui

import (
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

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
