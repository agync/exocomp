package web

import (
	"errors"
	"net/http"
	"strings"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/store"
)

// Job→Schedule turns a completed run into a recurring cron schedule without
// the operator retyping its command. The created schedule's kind is always
// "shell": it replays the job's own stored Argv, byte for byte, through the
// shell driver's direct exec — never the ansible driver's sandbox, vault, or
// run-secrets machinery a playbook/command job used, since store.ScheduleArgs
// (internal/store/schedule.go) carries none of that. A job that ran against a
// --source working directory (Args.Dir set) has nowhere for a shell schedule
// to put it, so scheduling that job is refused rather than silently dropping
// where it ran. Schedules carry no secret material (schedules_write.go's own
// header comment), so this lane is operator-scope only, same as the rest of
// the schedules surface — no reauth.
func init() { registerWrite((*Server).registerJobScheduleWrite) }

func (s *Server) registerJobScheduleWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /jobs/{id}/schedule", s.requireAuth(s.handleJobSchedule))
}

// canScheduleJob reports whether a job is the one shape handleJobSchedule can
// honestly turn into a shell schedule: an ansible run (playbook or command)
// with a command to replay and no source working directory to lose.
func canScheduleJob(j store.Job) bool {
	return j.Kind == executor.KindAnsible && j.Args.Dir == "" && len(j.Args.Argv) > 0
}

func (s *Server) handleJobSchedule(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	job, err := s.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !canScheduleJob(job) {
		http.Error(w, "this job cannot be replayed as a shell schedule: it needs a command and no --source working directory", http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	// The schedule-add form's ValidName gate, so an empty or spaced name from
	// the job-detail form is refused here rather than surfacing the store's
	// validation sentinel as a 500.
	if !store.ValidName(name) {
		http.Error(w, "invalid schedule name", http.StatusBadRequest)
		return
	}
	cronExpr := r.PostFormValue("cron")
	if err := sched.ValidateCron(cronExpr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	created, err := s.store.CreateSchedule(r.Context(), store.NewSchedule{
		Name:     name,
		CronExpr: cronExpr,
		Kind:     store.ScheduleShell,
		// Argv comes from the stored job, never a client-posted field: the
		// form shows it read-only for operator confidence but never submits
		// it, so a stale or tampered form can never schedule a different
		// command than the one this job actually ran.
		Args: store.ScheduleArgs{Argv: job.Args.Argv},
	})
	if errors.Is(err, store.ErrDuplicate) {
		http.Error(w, "a schedule named "+name+" already exists", http.StatusConflict)
		return
	}
	if errors.Is(err, store.ErrValidation) {
		http.Error(w, validationFlash(err), http.StatusBadRequest)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: job schedule", "job", job.ID, "schedule", created.Name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/schedules", "scheduled "+created.Name)
}

// validationFlash renders a store.ErrValidation error as a flash line without
// the store's wrapped "store: validation: store: ..." prefix, so the browser
// shows one actionable sentence instead of the doubled internal namespace.
func validationFlash(err error) string {
	msg := strings.TrimPrefix(err.Error(), "store: validation: store: ")
	msg = strings.TrimPrefix(msg, "store: validation: ")
	return msg
}
