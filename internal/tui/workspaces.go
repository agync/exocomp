package tui

import (
	"strconv"

	"charm.land/bubbles/v2/table"

	"exocomp.dev/exocomp/internal/store"
)

// workspaceStatus is per-workspace derived info the Workspaces screen needs
// beyond the store.Workspace row itself: the state of the two tofu
// operations it can run, whether the most recent plan found drift, and the
// job Enter should tail.
type workspaceStatus struct {
	LastPlanState  store.JobState
	LastApplyState store.JobState
	Drift          bool
	// LatestJobID is the most recent plan or apply job, whichever ran last;
	// zero means the workspace has never been planned or applied.
	LatestJobID int64
}

// workspacesTable wraps the bubbles table component with the Workspaces
// screen's fixed column layout.
type workspacesTable struct {
	model table.Model
}

func workspacesColumns() []table.Column {
	return []table.Column{
		{Title: "NAME", Width: 16},
		{Title: "SERIAL", Width: 8},
		{Title: "LAST PLAN", Width: 12},
		{Title: "LAST APPLY", Width: 12},
		{Title: "DRIFT", Width: 8},
	}
}

func newWorkspacesTable() workspacesTable {
	return workspacesTable{
		model: table.New(
			table.WithColumns(workspacesColumns()),
			table.WithFocused(true),
			table.WithHeight(10),
		),
	}
}

func (t *workspacesTable) Focus() { t.model.Focus() }

func (t *workspacesTable) SetRows(rows []table.Row) { t.model.SetRows(rows) }

func (t *workspacesTable) SetSize(width, height int) {
	t.model.SetWidth(width)
	t.model.SetHeight(height)
}

func (t workspacesTable) View() string { return t.model.View() }

func workspaceRows(workspaces []store.Workspace, status map[int64]workspaceStatus) []table.Row {
	rows := make([]table.Row, len(workspaces))
	for i, ws := range workspaces {
		s := status[ws.ID]
		rows[i] = table.Row{
			ws.Name,
			strconv.FormatInt(ws.Serial, 10),
			jobStateCell(s.LastPlanState),
			jobStateCell(s.LastApplyState),
			driftCell(s.Drift),
		}
	}
	return rows
}

// jobStateCell renders a job state that may not exist yet: a workspace with
// no plan or apply job shows "-" like every other unset cell.
func jobStateCell(state store.JobState) string {
	if state == "" {
		return emptyCell
	}
	return renderState(string(state))
}

// driftCell marks a workspace whose most recent plan reported changes. There
// is no state to color when a plan found nothing, so the absence of drift is
// the same plain "-" every other unset cell uses.
func driftCell(drift bool) string {
	if !drift {
		return emptyCell
	}
	return styleErr.Render("drift")
}
