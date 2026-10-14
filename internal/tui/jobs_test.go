package tui

import (
	"strings"
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

// TestJobRowsMarksUnsandboxedSource pins the TUI's only privileged marker: a
// run that opted out of the kernel sandbox carries the "unsandboxed" fact in
// its SOURCE cell (the web list's "tree · unsandboxed"), and a sandboxed run
// does not. Dropping the marker must red.
func TestJobRowsMarksUnsandboxedSource(t *testing.T) {
	rows := jobRows([]store.Job{
		{ID: 1, Kind: "ansible", State: store.StateSucceeded, Args: store.JobArgs{Source: "tree", Privileged: true}},
		{ID: 2, Kind: "ansible", State: store.StateSucceeded, Args: store.JobArgs{Source: "tree"}},
	})
	if got := stripANSI(rows[0][1]); !strings.Contains(got, "unsandboxed") {
		t.Errorf("privileged row source cell = %q, want the unsandboxed fact", got)
	}
	if got := stripANSI(rows[1][1]); strings.Contains(got, "unsandboxed") {
		t.Errorf("sandboxed row source cell = %q, must not claim unsandboxed", got)
	}
}

// TestJobRowsUnsandboxedMarkerSurvivesLongSource proves the marker is never the
// tail the fixed SOURCE column truncates: a source longer than the name budget
// still shows "unsandboxed" within sourceColWidth columns.
func TestJobRowsUnsandboxedMarkerSurvivesLongSource(t *testing.T) {
	rows := jobRows([]store.Job{
		{ID: 1, Kind: "ansible", State: store.StateSucceeded,
			Args: store.JobArgs{Source: strings.Repeat("x", 40), Privileged: true}},
	})
	got := stripANSI(rows[0][1])
	if !strings.Contains(got, "unsandboxed") {
		t.Errorf("long source dropped the marker: %q", got)
	}
	if w := rw(got); w > sourceColWidth {
		t.Errorf("source cell %q is %d columns, over the %d budget", got, w, sourceColWidth)
	}
}

// TestJobsTableShowsUnsandboxedAtBothWidths proves the marker reaches the
// rendered table at the 120 full and 80 degraded widths: SOURCE survives the
// projection, so the fact is never a column the degraded set drops.
func TestJobsTableShowsUnsandboxedAtBothWidths(t *testing.T) {
	rows := jobRows([]store.Job{
		{ID: 7, Kind: "ansible", State: store.StateSucceeded, Args: store.JobArgs{Source: "tree", Privileged: true}},
	})
	for _, width := range []int{120, 80} {
		tbl := newJobsTable()
		tbl.SetSize(width, 10)
		tbl.SetRows(rows)
		if got := stripANSI(tbl.View()); !strings.Contains(got, "unsandboxed") {
			t.Errorf("width %d jobs table missing the unsandboxed marker:\n%s", width, got)
		}
	}
}
