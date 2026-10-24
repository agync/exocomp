package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// The backups write surface is the browser form face of the store verbs
// store/backup.go:101-133 finally getting web controls (SPEC §4): run,
// enable/disable, edit retention, remove (asks first), add-target (adds AND
// queues the first backup — one verb, because a target that has never run is
// a false sense of safety), and the Restore DR verb. Every run goes through
// the one queue (AGENTS.md invariant 5): run/drill enqueue the same kind
// jobs `exocomp backup run` and a kind=drill schedule fire do, and the daemon
// that owns the real queue claims them. Targets carry no secret material of
// their own (the repository password is derived at run time on the daemon
// side), so this lane is plain operator-scope, like schedules — no reauth.

func init() { registerWrite((*Server).registerBackupsWrite) }

func (s *Server) registerBackupsWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /system/backups/add", s.requireAuth(s.handleBackupAdd))
	mux.HandleFunc("POST /system/backups/{name}/run", s.requireAuth(s.handleBackupRun))
	mux.HandleFunc("POST /system/backups/{name}/drill", s.requireAuth(s.handleBackupDrill))
	mux.HandleFunc("POST /system/backups/{name}/enable", s.requireAuth(s.handleBackupEnable))
	mux.HandleFunc("POST /system/backups/{name}/disable", s.requireAuth(s.handleBackupDisable))
	mux.HandleFunc("POST /system/backups/{name}/retention", s.requireAuth(s.handleBackupRetention))
	mux.HandleFunc("POST /system/backups/{name}/remove", s.requireAuth(s.handleBackupRemove))
	mux.HandleFunc("POST /system/backups/{name}/forget", s.requireAuth(s.handleBackupForget))
	mux.HandleFunc("POST /system/backups/restore", s.requireAuth(s.handleBackupRestore))
}

// The Add-target dialog's human refusal copy (forms/SPEC.md + copy.txt): one
// sentence in the field's terms, never the validator's words or a status
// code. The raw causes go to the log at each call site below.
const (
	backupNameCharsRefusal = "Names use letters, digits, dots, hyphens and underscores."
	backupNameEmpty        = "Give the target a name."
	backupNameSpace        = "Names can't have spaces."
	backupKindRefusal      = "Kind is restic_local, restic_s3 or git."
	backupRepoRequired     = "Give a repository — restic -r syntax, or a git remote."
	backupRetentionRefusal = "Retention counts are whole numbers — like daily 7 · weekly 4."
	backupRetentionNeg     = "Retention counts can't be negative."
	backupScheduleRefusal  = "That schedule can't be parsed — try 0 2 * * * * or @every 24h."
	backupAddSubmitRefusal = "Couldn't add the target — nothing was saved."
	backupRemoveConfirm    = "Nothing was removed — confirm the removal in the dialog."
	// backupForgetConfirm and backupForgetRefusal are the per-snapshot forget
	// verb's asks-first gates (SPEC §7 A12): the confirm dialog's hidden
	// confirmed=1 is the safety, and a snapshot that is not a forgettable row
	// of the target is refused. The forget verb NEVER lives on the Restore
	// drawer — a delete verb beside the restore radios is a mis-click from
	// removing the snapshot being restored.
	backupForgetConfirm = "Nothing was forgotten — confirm the forget in the dialog."
	backupForgetNewest  = "The newest snapshot can't be forgotten — it is the recovery point."
	backupForgetRefusal = "That snapshot is not on this target's history — refresh the page."
	// backupRestoreConfirmRefusal is the DR drawer's hard rule (c): the typed
	// id must match the selected snapshot's id. A wrong id rings the button
	// and refuses on the drawer, never a flash.
	backupRestoreConfirmRefusal = "Type the snapshot id shown on the selected snapshot to confirm."
)

// backupNameRefusal translates a bad target-name value the way the
// netgroup/source forms do: empty is the required sentence, a spaced name the
// spaces sentence, anything else the charset sentence.
func backupNameRefusal(name string) (msg, fix string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return backupNameEmpty, ""
	}
	if strings.Contains(name, " ") {
		return backupNameSpace, netgroupSlug(name)
	}
	return backupNameCharsRefusal, netgroupSlug(name)
}

func (s *Server) handleBackupAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	form := backupAddFormVM{
		Name: r.PostFormValue("name"), Kind: r.PostFormValue("kind"),
		Repository: r.PostFormValue("repository"),
		Daily:      r.PostFormValue("daily"), Weekly: r.PostFormValue("weekly"),
		KeepAll:      r.Form.Has("keep_all"),
		ScheduleCron: r.PostFormValue("schedule"),
	}
	// The sibling whose cadence prefilled the schedule field, re-derived on a
	// refusal so the meta line reads the same.
	form.Sibling = s.newestBackupSibling(r.Context())

	// Validate every field up front so all refusals render at once
	// (forms/SPEC.md); the daemon stays authoritative on uniqueness.
	var causes []string
	if !store.ValidName(form.Name) {
		form.ErrName, form.FixName = backupNameRefusal(form.Name)
		causes = append(causes, "name "+form.Name)
	}
	switch form.Kind {
	case backup.KindResticLocal, backup.KindResticS3, backup.KindGit:
	default:
		form.ErrKind = backupKindRefusal
		causes = append(causes, "kind "+form.Kind)
	}
	if strings.TrimSpace(form.Repository) == "" {
		form.ErrRepo = backupRepoRequired
		causes = append(causes, "empty repository")
	}
	if !form.KeepAll {
		for _, cell := range []struct {
			name, val string
		}{{"daily", form.Daily}, {"weekly", form.Weekly}} {
			if cell.val == "" {
				continue
			}
			if n, err := strconv.Atoi(cell.val); err != nil || n < 0 {
				form.ErrRetention = backupRetentionRefusal
				if err == nil && n < 0 {
					form.ErrRetention = backupRetentionNeg
				}
				causes = append(causes, cell.name+" "+cell.val)
				break
			}
		}
	}
	if form.ScheduleCron != "" {
		if err := sched.ValidateCron(form.ScheduleCron); err != nil {
			form.ErrSchedule = backupScheduleRefusal
			causes = append(causes, err.Error())
		}
	}

	if form.ErrName != "" || form.ErrKind != "" || form.ErrRepo != "" ||
		form.ErrRetention != "" || form.ErrSchedule != "" {
		switch {
		case form.ErrName != "":
			form.FirstInvalid = "name"
		case form.ErrKind != "":
			form.FirstInvalid = "kind"
		case form.ErrRepo != "":
			form.FirstInvalid = "repository"
		case form.ErrRetention != "":
			form.FirstInvalid = "daily"
		case form.ErrSchedule != "":
			form.FirstInvalid = "schedule"
		}
		s.log.Info("web: backup add refused", "err", strings.Join(causes, "; "), "by", ident.token.Name)
		s.backupsRefusal(w, r, &form, nil, nil)
		return
	}

	retention := ""
	if form.Kind != backup.KindGit {
		retention = form.RetentionValue()
	}
	created, err := s.store.CreateBackupTarget(r.Context(), store.BackupTarget{
		Name: form.Name, Kind: form.Kind, URI: form.Repository,
		Retention: retention, Enabled: true,
	})
	if errors.Is(err, store.ErrDuplicate) {
		s.log.Info("web: backup add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrName = "A target named " + form.Name + " already exists."
		form.FirstInvalid = "name"
		s.backupsRefusal(w, r, &form, nil, nil)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rollback := func() { _ = s.store.DeleteBackupTarget(r.Context(), created.ID) }

	// The schedule half of the one verb: a kind=backup schedule on /schedules
	// firing this target, created only when the operator gave a cadence. A
	// schedule-name collision rolls the target back so the dialog can refuse
	// cleanly instead of leaving half a target behind.
	if form.ScheduleCron != "" {
		if _, err := s.store.CreateSchedule(r.Context(), store.NewSchedule{
			Name: form.Name, CronExpr: form.ScheduleCron, Kind: store.ScheduleBackup,
			Args:      store.ScheduleArgs{Target: form.Name},
			CreatedBy: service.ActorLabel(service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name, Face: "web"}),
		}); err != nil {
			rollback()
			if errors.Is(err, store.ErrDuplicate) {
				form.ErrForm = "A schedule named " + form.Name + " already exists on /schedules — remove it or pick a different name."
			} else {
				form.ErrForm = backupAddSubmitRefusal
			}
			s.log.Info("web: backup add refused", "err", err.Error(), "by", ident.token.Name)
			s.backupsRefusal(w, r, &form, nil, nil)
			return
		}
		// An enqueue failure below must not leave a schedule pointing at a
		// target that no longer exists.
		rollback = func() {
			_ = s.store.DeleteBackupTarget(r.Context(), created.ID)
			_ = s.store.DeleteSchedule(r.Context(), form.Name)
		}
	}

	// The queue half: the first backup runs now. A failed enqueue rolls the
	// target back too — an added target with no run queued would read as safe
	// when it is not.
	job, err := s.enqueuer().Enqueue(r.Context(), backupRunSpec(created.Name))
	if err != nil {
		rollback()
		s.log.Info("web: backup add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrForm = backupAddSubmitRefusal
		s.backupsRefusal(w, r, &form, nil, nil)
		return
	}
	s.log.Info("web: backup add", "target", created.Name, "kind", created.Kind, "job", job.ID, "by", ident.token.Name)
	// A new target changes what the recovery kit prints (the target list), so
	// the kit is stale until it is reprinted (SPEC §7 A3).
	s.bumpKitChanges(r.Context())
	s.redirectFlash(w, r, "/system/backups",
		"added "+created.Name+" · backup queued as job #"+itoa64(job.ID))
}

// newestBackupSibling returns the newest existing target's name, the Add
// dialog's schedule-prefill source ("same cadence as {sibling}").
func (s *Server) newestBackupSibling(ctx context.Context) string {
	targets, err := s.store.ListBackupTargets(ctx, true)
	if err != nil || len(targets) == 0 {
		return ""
	}
	// ListBackupTargets orders by id ascending; the newest is the last.
	return targets[len(targets)-1].Name
}

// handleBackupRun enqueues one target's backup job through the one queue.
// A DISABLED target is refused on the row (the run verb is absent there, but
// a crafted POST must not start a disabled target) — the mutation gate.
func (s *Server) handleBackupRun(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	row, err := s.store.GetBackupTarget(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No backup target named "+name+" exists to run.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	if !row.Enabled {
		s.log.Info("web: backup run refused", "err", "disabled target "+name, "by", ident.token.Name)
		s.backupsRowRefusal(w, r, name, "Target "+name+" is disabled — enable it before running a backup.")
		return
	}
	job, err := s.enqueuer().Enqueue(r.Context(), backupRunSpec(name))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: backup run", "target", name, "job", job.ID, "by", ident.token.Name)
	s.redirectFlash(w, r, "/system/backups",
		"queued backup "+name+" as job #"+itoa64(job.ID))
}

// handleBackupDrill enqueues one target's restore-drill job — the same kind a
// kind=drill schedule fire enqueues, one queue.
func (s *Server) handleBackupDrill(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if _, err := s.store.GetBackupTarget(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No backup target named "+name+" exists to drill.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	job, err := s.enqueuer().Enqueue(r.Context(), backupDrillSpec(name))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: backup drill", "target", name, "job", job.ID, "by", ident.token.Name)
	s.redirectFlash(w, r, "/system/backups",
		"queued drill "+name+" as job #"+itoa64(job.ID))
}

func (s *Server) handleBackupEnable(w http.ResponseWriter, r *http.Request) {
	s.handleBackupSetEnabled(w, r, true)
}

func (s *Server) handleBackupDisable(w http.ResponseWriter, r *http.Request) {
	s.handleBackupSetEnabled(w, r, false)
}

// handleBackupSetEnabled flips a target's enabled flag through the same
// UpdateBackupTarget the store exposes — the ENABLED/DISABLED word on the row.
func (s *Server) handleBackupSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	row, err := s.store.GetBackupTarget(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			verb := "enable"
			if !enabled {
				verb = "disable"
			}
			http.Error(w, "No backup target named "+name+" exists to "+verb+".", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	row.Enabled = enabled
	if err := s.store.UpdateBackupTarget(r.Context(), row); err != nil {
		s.fail(w, r, err)
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	s.log.Info("web: backup "+verb, "target", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/system/backups", verb+" "+name)
}

// handleBackupRetention writes the edit-retention dialog's cells into the
// target's stored retention spec.
func (s *Server) handleBackupRetention(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	form := backupRetentionFormVM{
		Name: name, Daily: r.PostFormValue("daily"), Weekly: r.PostFormValue("weekly"),
		KeepAll: r.Form.Has("keep_all"),
	}
	if !form.KeepAll {
		for _, cell := range []struct {
			name, val string
		}{{"daily", form.Daily}, {"weekly", form.Weekly}} {
			if cell.val == "" {
				continue
			}
			if n, err := strconv.Atoi(cell.val); err != nil || n < 0 {
				if err == nil && n < 0 {
					form.ErrDaily = backupRetentionNeg
				} else {
					form.ErrDaily = backupRetentionRefusal
				}
				form.FirstInvalid = "daily"
				s.log.Info("web: backup retention refused", "target", name, "err", cell.name+" "+cell.val, "by", ident.token.Name)
				s.backupsRefusal(w, r, nil, &form, nil)
				return
			}
		}
	}
	row, err := s.store.GetBackupTarget(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No backup target named "+name+" exists.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	// A git target keeps history — retention does not apply, so refuse the
	// store update instead of pretending a policy for it.
	if row.Kind == backup.KindGit {
		s.log.Info("web: backup retention refused", "err", "git target "+name, "by", ident.token.Name)
		form.ErrForm = "Git targets keep history — retention does not apply."
		s.backupsRefusal(w, r, nil, &form, nil)
		return
	}
	row.Retention = form.RetentionValue()
	if err := s.store.UpdateBackupTarget(r.Context(), row); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: backup retention", "target", name, "retention", row.Retention, "by", ident.token.Name)
	s.redirectFlash(w, r, "/system/backups", "retention updated for "+name)
}

// handleBackupRemove deletes a target, asks first (the shared confirm
// dialog's hidden confirmed=1 is the safety).
func (s *Server) handleBackupRemove(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !r.Form.Has("confirmed") {
		s.backupsRowRefusal(w, r, name, backupRemoveConfirm)
		return
	}
	row, err := s.store.GetBackupTarget(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No backup target named "+name+" exists to remove.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	if err := s.store.DeleteBackupTarget(r.Context(), row.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: backup remove", "target", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/system/backups", "removed "+name)
}

// handleBackupForget is the per-snapshot forget verb (SPEC §7 A12): it asks
// first (the row's confirm dialog posts hidden confirmed=1, the same gate as
// remove), refuses a snapshot that is not a forgettable row of the target —
// the newest snapshot and any drilled one are never forgettable, and a
// crafted POST cannot name a snapshot that is not on the history page — and
// enqueues ONE forget job through the one queue (invariant 5). The execution
// of the kind is the daemon-side runner leg this lane's report flags; the web
// only ever names the target and snapshot through the queue.
func (s *Server) handleBackupForget(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	row, err := s.store.GetBackupTarget(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No backup target named "+name+" exists.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	if !r.Form.Has("confirmed") {
		s.historyRefusal(w, r, name, shortID(r.PostFormValue("snapshot")), backupForgetConfirm)
		return
	}
	snapshot := r.PostFormValue("snapshot")
	if refusal := s.forgetRefusal(r.Context(), row, snapshot); refusal != "" {
		s.log.Info("web: backup forget refused", "err", refusal, "target", name, "by", ident.token.Name)
		s.historyRefusal(w, r, name, shortID(snapshot), refusal)
		return
	}
	job, err := s.enqueuer().Enqueue(r.Context(), backupForgetSpec(name, snapshot))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: backup forget", "target", name, "snapshot", snapshot, "job", job.ID, "by", ident.token.Name)
	s.redirectFlash(w, r, "/system/backups/"+name+"/history",
		"queued forget of #"+shortID(snapshot)+" as job #"+itoa64(job.ID))
}

// forgetRefusal is the forget verb's snapshot gate: "" when the snapshot is a
// forgettable row of the target, else the sentence that names why — the
// newest/drilled recovery point, or a snapshot that is no row at all.
func (s *Server) forgetRefusal(ctx context.Context, t store.BackupTarget, snapshot string) string {
	rows, err := s.backupHistoryRows(ctx, t)
	if err != nil {
		return backupForgetRefusal
	}
	for _, row := range rows {
		if row.ID == shortID(snapshot) {
			if row.Forget {
				return ""
			}
			return backupForgetNewest
		}
	}
	return backupForgetRefusal
}

// historyRefusal re-renders the target history page at 422 with a refused
// forget verb's sentence on the named row (or the surface notice when the
// row is not on the page) — never a flash.
func (s *Server) historyRefusal(w http.ResponseWriter, r *http.Request, name, snapID, msg string) {
	vm, err := s.historyVMFor(r, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No backup target named "+name+" exists.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	placed := false
	for i := range vm.Snapshots {
		if snapID != "" && vm.Snapshots[i].ID == snapID {
			vm.Snapshots[i].Refusal = msg
			placed = true
		}
	}
	if !placed {
		vm.Notice = msg
	}
	s.render(w, r, http.StatusUnprocessableEntity, backupsHistory(vm))
}

// handleBackupRestore is the DR drawer's verb (SPEC §4.3, D3): it runs the
// round's hard rule (c) — the typed id must match the selected snapshot's id —
// and then REFUSES to fake a restore over the live store. A restore replaces
// the store this daemon runs on and needs the offline recovery identity
// (backup.RestoreLatest's checkDest refuses a destination that already holds a
// keyring), so the live web cannot execute it: a valid typed confirm changes
// the drawer body to the RUN ON THE HOST block — the recovery-kit command
// (`exocomp restore` / `--from-litestream`) with its Copy verb — re-rendered
// at 200 with the drawer open (JS reveals it in place; this is the JS-off
// path). A wrong id rings the button and refuses on the drawer at 422 — never
// a flash. Replica mode (m76's deep link) types the word "replica" through the
// same gate.
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	from := r.PostFormValue("from")
	selected := r.PostFormValue("snapshot")
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	form := &restoreFormVM{Mode: from, Selected: selected, Confirm: confirm, Open: true}
	if form.Mode == "" {
		form.Mode = "snapshot"
	}

	match := false
	if form.Mode == "replica" {
		// The litestream mode has no snapshot id to type; the confirm is the
		// word that names the source (m76's contract).
		match = strings.EqualFold(confirm, "replica")
	} else {
		for _, snap := range s.currentSnapshots(r.Context()) {
			if snap.ID == selected {
				match = confirm == selected
				break
			}
		}
	}
	if !match {
		s.log.Info("web: backup restore refused", "err", "typed confirm did not match", "by", ident.token.Name)
		form.ErrConfirm = backupRestoreConfirmRefusal
		s.backupsRefusal(w, r, nil, nil, form)
		return
	}
	s.log.Info("web: backup restore confirmed", "from", form.Mode, "snapshot", selected, "by", ident.token.Name)
	form.Confirmed = true
	vm, err := s.backupsVMFor(r, nil, nil, form)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, backupsList(vm))
}

// currentSnapshots is the drawer's radio source at submit time, so a typed id
// is checked against the snapshots the page actually listed (a stale or
// crafted POST cannot name a snapshot that is not on screen).
func (s *Server) currentSnapshots(ctx context.Context) []snapshotRow {
	snaps, err := s.backupSnapshots(ctx)
	if err != nil {
		return nil
	}
	return snaps
}

// backupsRefusal re-renders the /system/backups surface at 422 with the
// refused surface's state carried — the add dialog, one row's retention
// dialog, or the DR drawer (forms/SPEC.md button-verb shape): a refusal
// renders on the surface that holds the input, never a flash.
func (s *Server) backupsRefusal(w http.ResponseWriter, r *http.Request, add *backupAddFormVM, retention *backupRetentionFormVM, restore *restoreFormVM) {
	vm, err := s.backupsVMFor(r, add, retention, restore)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, backupsList(vm))
}

// backupsRowRefusal re-renders the surface at 422 with a refused row verb's
// refusal on the refused row.
func (s *Server) backupsRowRefusal(w http.ResponseWriter, r *http.Request, name, msg string) {
	vm, err := s.backupsVMFor(r, nil, nil, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for i := range vm.Targets {
		if vm.Targets[i].Name == name {
			vm.Targets[i].Refusal = msg
		}
	}
	s.render(w, r, http.StatusUnprocessableEntity, backupsList(vm))
}
