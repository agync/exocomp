package tui

import (
	"charm.land/bubbles/v2/table"

	"exocomp.dev/exocomp/internal/store"
)

// backupsTable wraps the bubbles table component with the Backups screen's
// fixed column layout.
type backupsTable struct {
	model table.Model
}

func backupsColumns() []table.Column {
	return []table.Column{
		{Title: "TARGET", Width: 16},
		{Title: "KIND", Width: 12},
		{Title: "STATE", Width: 12},
		{Title: "SNAPSHOT", Width: 14},
		{Title: "BYTES", Width: 9},
		{Title: "ERROR", Width: 24},
	}
}

func newBackupsTable() backupsTable {
	return backupsTable{
		model: table.New(
			table.WithColumns(backupsColumns()),
			table.WithFocused(true),
			table.WithHeight(10),
		),
	}
}

func (t *backupsTable) Focus() { t.model.Focus() }

func (t *backupsTable) SetRows(rows []table.Row) { t.model.SetRows(rows) }

func (t *backupsTable) SetSize(width, height int) {
	t.model.SetWidth(width)
	t.model.SetHeight(height)
}

func (t backupsTable) View() string { return t.model.View() }

func backupRows(targets []store.BackupTarget, runs map[int64]store.BackupRun) []table.Row {
	rows := make([]table.Row, len(targets))
	for i, target := range targets {
		if !target.Enabled {
			rows[i] = disabledBackupRow(target)
			continue
		}
		run, ok := runs[target.ID]
		if !ok {
			rows[i] = table.Row{target.Name, target.Kind, renderState("-"), emptyCell, emptyCell, ""}
			continue
		}
		snapshot := run.SnapshotID
		if snapshot == "" {
			snapshot = emptyCell
		}
		rows[i] = table.Row{
			target.Name,
			target.Kind,
			renderState(run.State),
			truncate(snapshot, 14),
			formatBytes(run.BytesAdded),
			truncate(run.Error, 24),
		}
	}
	return rows
}

// disabledBackupRow dims a disabled target end to end and reports "disabled"
// in STATE: `exocomp backup list` calls this same state disabled, and a
// disabled target's last run outcome (or its absence) is not its state.
func disabledBackupRow(target store.BackupTarget) table.Row {
	return table.Row{
		styleDim.Render(target.Name),
		styleDim.Render(target.Kind),
		styleDim.Render("disabled"),
		styleDim.Render(emptyCell),
		styleDim.Render(emptyCell),
		"",
	}
}
