package web

import (
	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/queue"
)

// backupRowActions is the /system/backups row-action set (SPEC §4.1): on an
// ENABLED row the run glyph (play, accent) then ▾; on a DISABLED row the run
// verb is ABSENT and only ▾ renders (the fixture's "[▾]" row), with enable
// inside the menu. The chevron never moves — it stays the fixed right slot —
// so both rows force the menu by marking every verb Menu (rowActions renders
// the chevron when the row holds no glyph slots). ▾ holds edit retention
// (modal) · disable|enable · drill now · remove (confirmed, last).
func backupRowActions(row backupTargetRow) []rowAction {
	acts := []rowAction{}
	if row.Enabled {
		acts = append(acts, rowAction{
			Name: "Queue backup — " + row.Name, Path: glyphPlay,
			Form:    "/system/backups/" + row.Name + "/run",
			Refusal: row.Refusal,
		})
	} else {
		// The run verb is absent on a DISABLED row; its slot carries nothing
		// and the chevron holds enable instead. The refusal lands on the menu's
		// enable item so a refused run attempt reads on the row (never a flash).
		acts = append(acts, rowAction{
			Name: "Enable", Path: glyphEnable, Menu: true,
			Form:    "/system/backups/" + row.Name + "/enable",
			Refusal: row.Refusal,
		})
	}
	if row.Enabled {
		acts = append(acts,
			rowAction{
				Name: "Edit retention", Path: glyphPencil, Menu: true,
				Modal: "retention-" + row.Name,
			},
			rowAction{
				Name: "Disable", Path: glyphDisable, Menu: true,
				Form: "/system/backups/" + row.Name + "/disable",
			},
		)
	} else {
		acts = append(acts,
			rowAction{
				Name: "Edit retention", Path: glyphPencil, Menu: true,
				Modal: "retention-" + row.Name,
			},
		)
	}
	acts = append(acts,
		rowAction{
			Name: "Drill now", Path: glyphDrill, Menu: true,
			Form: "/system/backups/" + row.Name + "/drill",
		},
		rowAction{
			Name: "Remove", Path: glyphRemove, Menu: true,
			Form:    "/system/backups/" + row.Name + "/remove",
			Hidden:  map[string]string{"confirmed": "1"},
			Confirm: "Remove backup target " + row.Name + "? This deletes its run history.",
			Danger:  true,
		},
	)
	return acts
}

// The backups row verbs that the fixed per-verb glyph set does not already
// name: the play glyph (SCREENS.txt GLYPHS "run"), the drill probe, and the
// retention pencil (the designer's arm/edit path).
const (
	glyphPlay   = "M7 4l12 8-12 8z"
	glyphDrill  = "M9 3h6v2H9zM7 5h10v2H7zM8 7h8l1 5H7zM9 12h2v6a2 2 0 0 1-4 0h2z"
	glyphPencil = "M4 20l4-1 10-10-3-3L5 16zM13 7l3 3"
)

// backupRunSpec is the queue.Spec a run verb enqueues — the same shape the
// CLI's `exocomp backup run --target {name}` builds (cmd/exocomp/backup.go):
// kind backup through the one queue (invariant 5), concurrency keyed so two
// backups of a target never overlap.
func backupRunSpec(name string) queue.Spec {
	return queue.Spec{
		Runbook: "backup", Kind: backup.JobKind, Trigger: "web",
		Argv: backup.Argv(name), ConcurrencyKey: backup.ConcurrencyKey(name),
	}
}

// backupDrillSpec is the queue.Spec a "drill now" verb enqueues, mirroring a
// kind=drill schedule fire (internal/sched): the same backup-drill job the
// schedule enqueues, keyed per target.
func backupDrillSpec(name string) queue.Spec {
	return queue.Spec{
		Runbook: "backup-drill", Kind: backup.DrillJobKind, Trigger: "web",
		Argv: backup.DrillArgv(name), ConcurrencyKey: backup.DrillConcurrencyKey(name),
	}
}
