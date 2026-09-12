package web

import (
	"net/http"

	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/service"
)

// The /system Doctor verb (SPEC §7 A1): the self-check has a web face only
// because it is a job. The glyph POSTs here, the handler enqueues a
// kind=doctor job through the one queue (invariant 5 — the web never runs the
// probe), and the daemon that owns the real queue claims and runs it. The job
// page renders the findings as rows; the identity strip reads the same job's
// age and finding count. `exocomp doctor` stays the scriptable twin.

func init() { registerWrite((*Server).registerDoctorWrite) }

func (s *Server) registerDoctorWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /system/doctor", s.requireAuth(s.handleDoctorRun))
}

// handleDoctorRun enqueues one self-check job and lands the operator on the
// job page, where the findings render as rows.
func (s *Server) handleDoctorRun(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	res, err := s.service().Reenqueue(r.Context(), s.enqueuer(), queue.Spec{
		Runbook: health.JobRunbook, Kind: health.JobKind, Trigger: "manual",
		Argv: health.Argv(),
	}, service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name, Face: "web"})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: doctor run", "job", res.Job.ID, "by", ident.token.Name)
	s.redirectFlash(w, r, "/jobs/"+itoa64(res.Job.ID),
		"self-check queued as job #"+itoa64(res.Job.ID))
}
