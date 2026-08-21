package tui

import (
	"strconv"

	"charm.land/bubbles/v2/table"

	"exocomp.dev/exocomp/internal/store"
)

// jobsTable wraps the bubbles table component with the Jobs screen's fixed
// column layout.
type jobsTable struct {
	model table.Model
}

func jobsColumns() []table.Column {
	return []table.Column{
		{Title: "ID", Width: 6},
		{Title: "KIND", Width: 12},
		{Title: "STATE", Width: 12},
		{Title: "QUEUED", Width: 10},
		{Title: "FINISHED", Width: 10},
		{Title: "EXIT", Width: 5},
	}
}

func newJobsTable() jobsTable {
	return jobsTable{
		model: table.New(
			table.WithColumns(jobsColumns()),
			table.WithFocused(true),
			table.WithHeight(10),
		),
	}
}

func (t *jobsTable) Focus() { t.model.Focus() }

func (t *jobsTable) SetRows(rows []table.Row) { t.model.SetRows(rows) }

func (t *jobsTable) SetSize(width, height int) {
	t.model.SetWidth(width)
	t.model.SetHeight(height)
}

func (t jobsTable) View() string { return t.model.View() }

func jobRows(jobs []store.Job) []table.Row {
	rows := make([]table.Row, len(jobs))
	for i, j := range jobs {
		rows[i] = table.Row{
			strconv.FormatInt(j.ID, 10),
			j.Kind,
			renderState(string(j.State)),
			formatTime(j.QueuedAt),
			formatTimePtr(j.FinishedAt),
			formatExitCode(j.ExitCode),
		}
	}
	return rows
}
