package tui

import (
	"charm.land/bubbles/v2/table"

	"exocomp.dev/exocomp/internal/store"
)

// schedulesTable wraps the bubbles table component with the Schedules
// screen's fixed column layout.
type schedulesTable struct {
	model table.Model
}

func schedulesColumns() []table.Column {
	return []table.Column{
		{Title: "NAME", Width: 16},
		{Title: "CRON", Width: 14},
		{Title: "KIND", Width: 8},
		{Title: "ENABLED", Width: 8},
		{Title: "NEXT", Width: 10},
		{Title: "LAST STATE", Width: 12},
	}
}

func newSchedulesTable() schedulesTable {
	return schedulesTable{
		model: table.New(
			table.WithColumns(schedulesColumns()),
			table.WithFocused(true),
			table.WithHeight(10),
		),
	}
}

func (t *schedulesTable) Focus() { t.model.Focus() }

func (t *schedulesTable) SetRows(rows []table.Row) { t.model.SetRows(rows) }

func (t *schedulesTable) SetSize(width, height int) {
	t.model.SetWidth(width)
	t.model.SetHeight(height)
}

func (t schedulesTable) View() string { return t.model.View() }

func scheduleRows(schedules []store.Schedule, lastStates map[int64]store.JobState) []table.Row {
	rows := make([]table.Row, len(schedules))
	for i, sc := range schedules {
		if !sc.Enabled {
			rows[i] = disabledScheduleRow(sc, lastStates[sc.ID])
			continue
		}
		rows[i] = table.Row{
			sc.Name,
			sc.CronExpr,
			sc.Kind,
			"yes",
			formatTimePtr(sc.NextAt),
			jobStateCell(lastStates[sc.ID]),
		}
	}
	return rows
}

// disabledScheduleRow dims a disabled schedule end to end, mirroring
// disabledBackupRow: `Scheduler.Reload` (internal/sched) skips a disabled
// schedule, so its cron and last outcome are historical rather than
// something that will fire again until re-enabled.
func disabledScheduleRow(sc store.Schedule, lastState store.JobState) table.Row {
	text := emptyCell
	if lastState != "" {
		text = string(lastState)
	}
	return table.Row{
		styleDim.Render(sc.Name),
		styleDim.Render(sc.CronExpr),
		styleDim.Render(sc.Kind),
		styleDim.Render("no"),
		styleDim.Render(formatTimePtr(sc.NextAt)),
		styleDim.Render(text),
	}
}
