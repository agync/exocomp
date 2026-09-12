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

// backupHistoryRowActions is one history-page snapshot row's action set
// (SPEC §7 A12): forget under the row's ▾, in fail, asks first, and only on a
// forgettable row — the newest and any drilled snapshot carry no verb, so the
// cell renders nothing at all there. A refused forget's sentence renders on
// the row itself (backupsHistory), never on an absent action.
func backupHistoryRowActions(snap historySnapRow, name string) []rowAction {
	acts := []rowAction{}
	if snap.Forget {
		acts = append(acts, rowAction{
			Name: "Forget snapshot #" + snap.ID,
			Path: glyphRemove, Menu: true,
			Form:    "/system/backups/" + name + "/forget",
			Hidden:  map[string]string{"snapshot": snap.FullID, "confirmed": "1"},
			Confirm: "Forget snapshot #" + snap.ID + " of " + name + "? restic forget removes it from the repository.",
			Danger:  true,
		})
	}
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

// The forget verb's queue shape (SPEC §7 A12, invariant 5): forgetJobKind is
// the queue.Spec/executor.Spec kind a per-snapshot forget carries, and
// forgetArgv0 the legible argv[0] the job row shows (never exec'd — the same
// convention as JobKind/DrillJobKind). The argv names the target and the FULL
// snapshot id; the job shares the target's backup concurrency key so a forget
// can never race a backup or a retention prune of the same repository. The
// daemon-side runner leg for this kind is the follow-up this lane reports.
const (
	forgetJobKind = "backup-forget"
	forgetArgv0   = "exocomp-forget"
)

// ForgetArgv builds the argv a queued forget job carries for one target and
// one snapshot id.
func ForgetArgv(target, snapshot string) []string {
	return []string{forgetArgv0, target, snapshot}
}

// backupForgetSpec is the queue.Spec the forget verb enqueues: one job
// through the one queue (invariant 5), concurrency-keyed like a backup so the
// repository lock never sees a forget and a backup/prune at the same time.
func backupForgetSpec(name, snapshot string) queue.Spec {
	return queue.Spec{
		Runbook: forgetJobKind, Kind: forgetJobKind, Trigger: "web",
		Argv: ForgetArgv(name, snapshot), ConcurrencyKey: backup.ConcurrencyKey(name),
	}
}

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
