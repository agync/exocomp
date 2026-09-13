package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/store"
)

// This file is the backups leg's read side (SPEC §4): the /system/backups
// list rows, the cross-target snapshot list for the Restore drawer (fold
// S-3), the drill outcome read (SPEC §7 A4), and the per-target snapshot
// history page (A12). The restic/git machinery stays internal/backup's — the
// web reads the CRUD and the run rows, never a repository.

// handleBackups renders the /system/backups management list (SPEC §4.1).
// ?restore=replica is the L-S3 deep link: it opens the Restore drawer in
// replica mode (the litestream leg's "restore from replica"), the drawer
// contract m76 keys off.
func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	var restore *restoreFormVM
	if r.URL.Query().Get("restore") == "replica" || r.URL.Query().Get("from") == "litestream" {
		restore = &restoreFormVM{Mode: "replica", Open: true}
	}
	vm, err := s.backupsVMFor(r, nil, nil, restore)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, backupsList(vm))
}

// backupsVMFor builds the /system/backups render state, shared by the GET
// handler and the verb-refusal re-renders so a refused verb shows the same
// list the operator left, with the refusal on it. addForm is the add dialog's
// state (nil at rest), retentionForm one row's edit-retention state (nil at
// rest), and restore the DR drawer's state (nil at rest).
func (s *Server) backupsVMFor(r *http.Request, addForm *backupAddFormVM, retentionForm *backupRetentionFormVM, restore *restoreFormVM) (backupsVM, error) {
	ident, _ := identityOf(r.Context())
	vm := backupsVM{
		Page: s.pageFor(r, "System · Backups", "system"), Operator: canWrite(ident),
		AddForm: addForm, RetentionForm: retentionForm, Restore: restore,
	}
	rows, err := s.backupListRows(r.Context())
	if err != nil {
		return backupsVM{}, err
	}
	vm.Targets = rows
	snaps, err := s.backupSnapshots(r.Context())
	if err != nil {
		return backupsVM{}, err
	}
	vm.Snapshots = snaps
	// The drawer defaults to snapshot mode with the newest snapshot selected;
	// a deep link (m76's replica mode) overrides Mode only.
	if vm.Restore == nil {
		vm.Restore = &restoreFormVM{Mode: "snapshot"}
	}
	if vm.Restore.Snapshots == nil {
		vm.Restore.Snapshots = snaps
	}
	if vm.Restore.Selected == "" && len(snaps) > 0 {
		vm.Restore.Selected = snaps[0].ID
	}
	if vm.Restore.Lag == "" {
		vm.Restore.Lag = s.selfObs(r.Context()).LSAge
	}
	if vm.Restore.LossAge == "" {
		if sel := vm.Restore.SelectedSnapshot(); sel != nil {
			vm.Restore.LossAge = sel.Age
		}
	}
	if vm.Restore.Waiting == 0 {
		if apps, err := s.store.ListApprovals(r.Context(), true); err == nil {
			vm.Restore.Waiting = len(apps)
		}
	}
	if vm.Restore.RunningJobs == 0 {
		if jobs, err := s.store.ListJobs(r.Context(), store.JobFilter{States: []store.JobState{store.StateRunning}}); err == nil {
			vm.Restore.RunningJobs = len(jobs)
		}
	}
	return vm, nil
}

// handleBackupHistory renders one target's snapshot history page (SPEC §7
// A4/A12): one snapshot per row with forget under the forgettable rows' ▾.
// The TARGET name on the management list links here.
func (s *Server) handleBackupHistory(w http.ResponseWriter, r *http.Request) {
	vm, err := s.historyVMFor(r, r.PathValue("name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No backup target named "+r.PathValue("name")+" exists.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, backupsHistory(vm))
}

// historyVMFor builds the target history page's render state. GetBackupTarget
// reports a missing target as ErrNotFound, which the handlers turn into a
// 404.
func (s *Server) historyVMFor(r *http.Request, name string) (backupsHistoryVM, error) {
	target, err := s.store.GetBackupTarget(r.Context(), name)
	if err != nil {
		return backupsHistoryVM{}, err
	}
	ident, _ := identityOf(r.Context())
	vm := backupsHistoryVM{
		Page:     s.pageFor(r, "System · Backups · "+target.Name, "system"),
		Operator: canWrite(ident),
		Target:   target.Name, Kind: target.Kind, Enabled: target.Enabled,
	}
	vm.Page.BackHref = s.url("/system/backups")
	vm.Page.BackLabel = "targets"
	rows, err := s.backupHistoryRows(r.Context(), target)
	if err != nil {
		return backupsHistoryVM{}, err
	}
	vm.Snapshots = rows
	return vm, nil
}

// backupHistoryRows reads one target's snapshots into history rows (SPEC §7
// A12): every SUCCEEDED run that produced a snapshot id, newest first. The
// drilled mark and its outcome land on the newest row when the target's
// newest drill job succeeded (A4). Forget renders only on a forgettable row —
// never the newest snapshot, never a drilled one.
func (s *Server) backupHistoryRows(ctx context.Context, t store.BackupTarget) ([]historySnapRow, error) {
	drillAge, drillBytes, drillDuration := s.drillFacts(ctx, t.Name)
	runs, err := s.store.ListBackupRuns(ctx, t.ID, 0)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []historySnapRow
	for _, run := range runs {
		if run.State != store.BackupSucceeded || run.SnapshotID == "" {
			continue
		}
		row := historySnapRow{
			ID: shortID(run.SnapshotID), FullID: run.SnapshotID,
			Age: humanAge(now.Sub(runFinishedAt(run))),
		}
		if run.BytesAdded > 0 {
			row.Size = humanBytes(int(run.BytesAdded))
		}
		// A drill verifies the newest snapshot at drill time, so only the
		// newest row of a drilled target carries the mark and its outcome.
		if len(out) == 0 && drillAge != "" {
			row.Newest = true
			row.Drilled = true
			row.DrillAge = drillAge
			row.DrillBytes = drillBytes
			row.DrillDuration = drillDuration
		}
		// Forget is ABSENT on the newest snapshot and on any drilled one
		// (SPEC §7 A12): the newest is the recovery point and a drilled one is
		// the one the drill just proved. Either guard alone blocks the row.
		row.Forget = len(out) > 0 && !row.Drilled
		out = append(out, row)
	}
	return out, nil
}

// backupListRows enriches every target into its tray-table row: posture
// (retention, schedule, URI scrubbed of userinfo) and the newest run's LAST
// RUN facts, the drill mark from the target's newest drill job, and the
// running job id for A9's `● RUNNING · job #…`.
func (s *Server) backupListRows(ctx context.Context) ([]backupTargetRow, error) {
	targets, err := s.store.ListBackupTargets(ctx, true)
	if err != nil {
		return nil, err
	}
	schedByTarget, err := s.backupSchedules(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]backupTargetRow, 0, len(targets))
	for _, t := range targets {
		row := backupTargetRow{
			Name: t.Name, Kind: t.Kind, URI: backup.ScrubURIUserinfo(t.URI),
			Retention: t.Retention, Enabled: t.Enabled, Base: s.basePath(),
			State: "never",
		}
		if sc := schedByTarget[t.Name]; sc != nil {
			row.Cadence = sc.CronExpr
			if sc.NextAt != nil {
				row.Next = "next in " + countdown(sc.NextAt.Sub(now))
			}
		}
		row.FillDrill(ctx, s)
		runs, err := s.store.ListBackupRuns(ctx, t.ID, 0)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			row.HasRun = true
			run := runs[0]
			row.State = run.State
			row.Snapshot = shortID(run.SnapshotID)
			if run.FinishedAt != nil {
				row.Age = humanAge(now.Sub(*run.FinishedAt))
				row.Duration = runDuration(run.FinishedAt.Sub(run.StartedAt))
			} else {
				row.Age = humanAge(now.Sub(run.StartedAt))
				row.Duration = durationClock(now.Sub(run.StartedAt))
			}
			if run.BytesAdded > 0 {
				row.Bytes = humanBytes(int(run.BytesAdded))
			}
			row.Reason = run.Error
			row.FailedSeq = consecutiveFailures(runs)
		}
		if row.JobID == 0 {
			// A RUNNING run's job is the newest for the target's
			// "backup:<name>" key: the job opens the run the moment it is
			// claimed. The job is read regardless of state so a mid-transition
			// or just-settled job still names the run (A9).
			row.JobID = s.newestJobID(ctx, backup.ConcurrencyKey(t.Name))
		}
		out = append(out, row)
	}
	return out, nil
}

// FillDrill reads the target's newest succeeded drill job into the row: a
// succeeded drill marks the newest snapshot "drilled {age} ago ok" and, when
// the job left its outcome event, carries the restored bytes + duration
// (A4). drillFacts is the one read every drill fact draws.
func (r *backupTargetRow) FillDrill(ctx context.Context, s *Server) {
	age, bytes, duration := s.drillFacts(ctx, r.Name)
	if age == "" {
		return
	}
	r.DrillOK = true
	r.DrillAge = age
	r.DrillBytes = bytes
	r.DrillDuration = duration
}

// drillFacts reads the target's newest succeeded drill job's outcome (SPEC
// §7 A4): the ok mark's age plus, read from the job's own "restore drill OK:
// …" outcome event's structured raw_json payload — the only trace a drill
// leaves, it never writes a backup_run row — the restored bytes and the
// drill's wall time. All three are empty when no succeeded drill (or no
// payload) exists. The exit code stays the judge; the payload only enriches
// the UI (invariant 3).
func (s *Server) drillFacts(ctx context.Context, name string) (age, bytes, duration string) {
	jobs, err := s.store.ListJobs(ctx, store.JobFilter{ConcurrencyKey: backup.DrillConcurrencyKey(name)})
	if err != nil || len(jobs) == 0 {
		return "", "", ""
	}
	drill := jobs[len(jobs)-1] // ListJobs is oldest-first; the last is the newest.
	if drill.State != store.StateSucceeded {
		return "", "", ""
	}
	age = humanAge(s.now().Sub(jobFinishedAt(drill)))
	if events, err := s.store.ListJobEvents(ctx, drill.ID); err == nil {
		for _, ev := range events {
			if outcome, ok := backup.DrillOutcomeFor(ev.RawJSON); ok {
				bytes = humanBytes(int(outcome.VerifiedBytes))
				duration = runDuration(drillDuration(drill))
				break
			}
		}
	}
	return age, bytes, duration
}

// drillDuration is a settled drill job's own wall time — the A4 duration
// fact. A succeeded job always has both stamps; a crash-reconciled row
// reports zero rather than a fabricated span.
func drillDuration(drill store.Job) time.Duration {
	if drill.StartedAt != nil && drill.FinishedAt != nil {
		return drill.FinishedAt.Sub(*drill.StartedAt)
	}
	return 0
}

// newestJobID returns the newest job id for a concurrency key, 0 when none.
func (s *Server) newestJobID(ctx context.Context, key string) int64 {
	jobs, err := s.store.ListJobs(ctx, store.JobFilter{ConcurrencyKey: key})
	if err != nil || len(jobs) == 0 {
		return 0
	}
	return jobs[len(jobs)-1].ID
}

// jobFinishedAt is the job's settlement time for a drill-age fact.
func jobFinishedAt(j store.Job) time.Time {
	if j.FinishedAt != nil {
		return *j.FinishedAt
	}
	if j.StartedAt != nil {
		return *j.StartedAt
	}
	return j.QueuedAt
}

// consecutiveFailures counts the newest run's streak of failed state.
func consecutiveFailures(runs []store.BackupRun) int {
	n := 0
	for _, run := range runs {
		if run.State != store.BackupFailed {
			break
		}
		n++
	}
	return n
}

// backupSchedules maps each target name to the kind=backup schedule that
// fires it — the SCHEDULE cell's cadence and the add dialog's sibling
// prefill. A schedule with an empty Target fans out to every target, so it
// does not name one.
func (s *Server) backupSchedules(ctx context.Context) (map[string]*store.Schedule, error) {
	list, err := s.store.ListSchedules(ctx, false)
	if err != nil {
		return nil, err
	}
	out := map[string]*store.Schedule{}
	for i := range list {
		sc := &list[i]
		if sc.Kind != store.ScheduleBackup || sc.Args.Target == "" {
			continue
		}
		out[sc.Args.Target] = sc
	}
	return out, nil
}

// backupSnapshots is the cross-target snapshot read the Restore drawer's
// radios draw (fold S-3): every SUCCEEDED run that produced a snapshot id,
// newest first across targets, with the drilled ok mark on the newest
// snapshot of a target whose newest drill job succeeded. restore.go scans a
// single repository internally; this is the newest-first list the web owns.
// The count the "all {n} snapshots" line shows is the same list's length.
func (s *Server) backupSnapshots(ctx context.Context) ([]snapshotRow, error) {
	targets, err := s.store.ListBackupTargets(ctx, true)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []snapshotRow
	for _, t := range targets {
		drilled, drillAge := s.targetDrilled(ctx, t.Name)
		runs, err := s.store.ListBackupRuns(ctx, t.ID, 0)
		if err != nil {
			return nil, err
		}
		for i, run := range runs {
			if run.State != store.BackupSucceeded || run.SnapshotID == "" {
				continue
			}
			row := snapshotRow{
				ID: shortID(run.SnapshotID), Target: t.Name,
				Age: humanAge(now.Sub(runFinishedAt(run))),
			}
			if run.BytesAdded > 0 {
				row.Size = humanBytes(int(run.BytesAdded))
			}
			// A drill verifies the newest snapshot at drill time, so only the
			// newest row of a drilled target carries the ok mark.
			if i == 0 && drilled {
				row.Drilled = true
				row.DrillAge = drillAge
			}
			out = append(out, row)
		}
	}
	return out, nil
}

// targetDrilled reports whether the target's newest backup-drill job
// succeeded, and how long ago, the "drilled {age} ago ok" mark's data — the
// same drillFacts read the LAST RUN cell and the history page draw.
func (s *Server) targetDrilled(ctx context.Context, name string) (bool, string) {
	age, _, _ := s.drillFacts(ctx, name)
	if age == "" {
		return false, ""
	}
	return true, age
}

// runFinishedAt is the settled instant of a succeeded run (finished_at when
// recorded, else its start — a run can succeed without a finish stamp in a
// crash-reconciled row).
func runFinishedAt(run store.BackupRun) time.Time {
	if run.FinishedAt != nil {
		return *run.FinishedAt
	}
	return run.StartedAt
}

// shortID renders a snapshot id at the length a human scans and the typed
// confirm matches — the same first-12-chars cut the CLI's list uses.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// countdown renders a duration the "next in {t}" way the SCHEDULE cell reads:
// sub-hour minutes, sub-day hours+minutes, days+hours beyond.
func countdown(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h >= 24:
		return fmt.Sprintf("%dd %02dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// durationClock renders a run's elapsed or wall time as HH:MM:SS — the
// RUNNING row's "00:02:10" (A9) and the settled run's duration.
func durationClock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", total/3600, (total/60)%60, total%60)
}

// truncateMeta shortens a subprocess reason line to what a LAST RUN cell can
// hold; the job page holds the full text.
func truncateMeta(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n-1]) + "…"
}

// lastRunReasonCap is how much of a subprocess reason a LAST RUN cell shows
// before the ellipsis.
const lastRunReasonCap = 64
