package tui

import (
	"charm.land/bubbles/v2/table"

	"exocomp.dev/exocomp/internal/store"
)

// schedulesTable wraps the bubbles table component with the Schedules
// screen's fixed column layout.
type schedulesTable struct {
	model    table.Model
	fullRows []table.Row
}

func schedulesColumns(width int) []table.Column {
	if width < degradedWidth {
		return []table.Column{
			{Title: "NAME", Width: 16},
			{Title: "KIND", Width: 10}, // fits the post-v1 kinds: tofu-plan, play, backup
			{Title: "CRON", Width: 14},
			{Title: "ENABLED", Width: 8},
			{Title: "NEXT", Width: 10},
		}
	}
	return []table.Column{
		{Title: "NAME", Width: 16},
		{Title: "KIND", Width: 10}, // fits the post-v1 kinds: tofu-plan, play, backup
		{Title: "CRON", Width: 14},
		{Title: "ENABLED", Width: 8},
		{Title: "NEXT", Width: 10},
		{Title: "FIRED", Width: 10},
		{Title: "LAST STATE", Width: 12},
	}
}

// schedulesKeep is the full-column indices that survive at degraded width:
// NAME, KIND, CRON, ENABLED, NEXT.
var schedulesKeep = []int{0, 1, 2, 3, 4}

func newSchedulesTable() schedulesTable {
	return schedulesTable{
		model: table.New(
			table.WithColumns(schedulesColumns(defaultWidth)),
			table.WithFocused(true),
			table.WithHeight(10),
		),
	}
}

func (t *schedulesTable) Focus() { t.model.Focus() }

func (t *schedulesTable) SetRows(rows []table.Row) {
	t.fullRows = rows
	t.model.SetRows(projectRows(t.model.Width(), rows, schedulesKeep))
}

func (t *schedulesTable) SetSize(width, height int) {
	setColumnsAndRows(&t.model, schedulesColumns(width), projectRows(width, t.fullRows, schedulesKeep))
	t.model.SetWidth(width)
	t.model.SetHeight(height)
}

func (t schedulesTable) View() string { return t.model.View() }

func scheduleRows(schedules []store.Schedule, last map[int64]scheduleLast) []table.Row {
	rows := make([]table.Row, len(schedules))
	for i, sc := range schedules {
		l := last[sc.ID]
		if !sc.Enabled {
			rows[i] = disabledScheduleRow(sc, l)
			continue
		}
		rows[i] = table.Row{
			sc.Name,
			sc.Kind,
			sc.CronExpr,
			"yes",
			formatTimePtr(sc.NextAt),
			scheduleFiredCell(l),
			jobStateCell(l.state),
		}
	}
	return rows
}

// scheduleFiredCell renders when a schedule last fired; a schedule that has
// never fired shows the same "-" every unset cell uses.
func scheduleFiredCell(l scheduleLast) string {
	if l.fired.IsZero() {
		return emptyCell
	}
	return formatTime(l.fired)
}

// disabledScheduleRow dims a disabled schedule end to end, mirroring
// disabledBackupRow: `Scheduler.Reload` (internal/sched) skips a disabled
// schedule, so its cron and last outcome are historical rather than
// something that will fire again until re-enabled.
func disabledScheduleRow(sc store.Schedule, l scheduleLast) table.Row {
	text := emptyCell
	if l.state != "" {
		text = string(l.state)
	}
	return table.Row{
		styleDim.Render(sc.Name),
		styleDim.Render(sc.Kind),
		styleDim.Render(sc.CronExpr),
		styleDim.Render("no"),
		styleDim.Render(formatTimePtr(sc.NextAt)),
		styleDim.Render(scheduleFiredCell(l)),
		styleDim.Render(text),
	}
}
