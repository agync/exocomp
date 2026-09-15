package tui

import (
	"strconv"

	"charm.land/bubbles/v2/table"

	"exocomp.dev/exocomp/internal/store"
)

// jobsTable wraps the bubbles table component with the Jobs screen's fixed
// column layout.
type jobsTable struct {
	model    table.Model
	fullRows []table.Row
}

// keepCells projects a full-width row onto the surviving columns. The degraded
// sets drop columns from the middle (jobs drops KIND), so a prefix trim would
// keep the wrong cells; the surviving indices are explicit.
func keepCells(row table.Row, keep []int) table.Row {
	out := make(table.Row, len(keep))
	for i, j := range keep {
		if j < len(row) {
			out[i] = row[j]
		}
	}
	return out
}

// projectRows maps full rows onto the width's column set: unchanged at full
// width, each row trimmed to the surviving indices below it.
func projectRows(width int, full []table.Row, keep []int) []table.Row {
	if width >= degradedWidth {
		return full
	}
	rows := make([]table.Row, len(full))
	for i, r := range full {
		rows[i] = keepCells(r, keep)
	}
	return rows
}

// setColumnsAndRows swaps the column set and the matching rows together. The
// bubbles table re-renders rows inside both SetColumns and SetRows, and
// renderRow indexes the row cells against the columns, so a row with more cells
// than columns panics. Each projection keeps every row the same length as the
// column set, so ordering by direction keeps every intermediate state
// consistent: shrink the rows first, grow the columns first, and touch neither
// while there are no rows (SetRows(nil) would move the cursor to -1).
func setColumnsAndRows(m *table.Model, cols []table.Column, rows []table.Row) {
	switch {
	case len(rows) == 0:
		m.SetColumns(cols)
	case len(rows) <= len(m.Columns()):
		m.SetRows(rows)
		m.SetColumns(cols)
	default:
		m.SetColumns(cols)
		m.SetRows(rows)
	}
}

func jobsColumns(width int) []table.Column {
	if width < degradedWidth {
		return []table.Column{
			{Title: "ID", Width: 6},
			{Title: "SOURCE", Width: 12},
			{Title: "STATE", Width: 12},
			{Title: "EXIT", Width: 5},
		}
	}
	return []table.Column{
		{Title: "ID", Width: 6},
		{Title: "SOURCE", Width: 12},
		{Title: "KIND", Width: 12},
		{Title: "STATE", Width: 12},
		{Title: "QUEUED", Width: 10},
		{Title: "FINISHED", Width: 10},
		{Title: "EXIT", Width: 5},
	}
}

// jobsKeep is the full-column indices that survive at degraded width:
// ID, SOURCE, STATE, EXIT.
var jobsKeep = []int{0, 1, 3, 6}

func newJobsTable() jobsTable {
	return jobsTable{
		model: table.New(
			table.WithColumns(jobsColumns(defaultWidth)),
			table.WithFocused(true),
			table.WithHeight(10),
		),
	}
}

func (t *jobsTable) Focus() { t.model.Focus() }

func (t *jobsTable) SetRows(rows []table.Row) {
	t.fullRows = rows
	t.model.SetRows(projectRows(t.model.Width(), rows, jobsKeep))
}

func (t *jobsTable) SetSize(width, height int) {
	setColumnsAndRows(&t.model, jobsColumns(width), projectRows(width, t.fullRows, jobsKeep))
	t.model.SetWidth(width)
	t.model.SetHeight(height)
}

func (t jobsTable) View() string { return t.model.View() }

func jobRows(jobs []store.Job) []table.Row {
	rows := make([]table.Row, len(jobs))
	for i, j := range jobs {
		rows[i] = table.Row{
			strconv.FormatInt(j.ID, 10),
			orDash(j.Args.Source),
			j.Kind,
			renderState(string(j.State)),
			formatTime(j.QueuedAt),
			formatTimePtr(j.FinishedAt),
			formatExitCode(j.ExitCode),
		}
	}
	return rows
}
