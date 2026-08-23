package web

import (
	"context"
	"net/http"

	"exocomp.dev/exocomp/internal/wire"
)

// This file mounts the drain/cordon maintenance action on the ADR-0009 listener,
// the operator-scoped channel the CLI already reaches the daemon through. It is
// the one place an operator can cordon a RUNNING daemon ahead of a planned
// reboot: cordon stops new schedule fires and new job claims while in-flight work
// finishes, and readiness answers "safe to reboot yet?" for the CLI to poll.
//
// Cordon and uncordon are operator-scope writes (canWrite, bearer-only): a
// readonly token — or an ambient browser session — is refused, the same floor
// plan/apply sit behind (agent-interface §4). Readiness and the interrupted
// listing are reads (canRead). None of this goes through the job queue: a cordon
// is daemon control, not a run, so there is no Spec to enqueue — the "queue/
// service pattern where applicable" does not apply to a control-plane toggle.

// Maintenance is the running daemon's drain-control seam. The concrete
// implementation lives in the daemon process (it pauses the scheduler and closes
// the queue's claim gate); the web surface only drives it. State also reports how
// many of the daemon's jobs are still in flight, which is what makes the
// readiness answer meaningful.
type Maintenance interface {
	Cordon(ctx context.Context) (MaintenanceState, error)
	Uncordon(ctx context.Context) (MaintenanceState, error)
	State(ctx context.Context) (MaintenanceState, error)
}

// MaintenanceState is a point-in-time drain reading: whether the daemon is
// cordoned and how many jobs it still owns in the running state.
type MaintenanceState struct {
	Cordoned bool
	InFlight int
}

// registerMaintenanceAPI mounts the /api/v1/maintenance routes. Cordon/uncordon
// carry the operator write floor; the reads carry the readonly floor.
func (s *Server) registerMaintenanceAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/maintenance/cordon",
		s.apiGuard(bearerOnly(canWrite), writeDeny("cordon the daemon", "operator"), s.apiCordon))
	mux.HandleFunc("POST /api/v1/maintenance/uncordon",
		s.apiGuard(bearerOnly(canWrite), writeDeny("uncordon the daemon", "operator"), s.apiUncordon))
	mux.HandleFunc("GET /api/v1/maintenance/readiness", s.apiAuth(s.apiReadiness))
	mux.HandleFunc("GET /api/v1/maintenance/interrupted", s.apiAuth(s.apiInterrupted))
}

// maintOrUnavailable returns the daemon's maintenance seam, or renders a 503 and
// reports false when this surface has none (a standalone web process runs no
// scheduler or queue, so it cannot cordon anything).
func (s *Server) maintOrUnavailable(w http.ResponseWriter) (Maintenance, bool) {
	if s.maint == nil {
		s.apiError(w, http.StatusServiceUnavailable, wire.NewError(wire.CodeInternal,
			"this surface runs no job queue or scheduler; cordon the daemon that does"))
		return nil, false
	}
	return s.maint, true
}

func (s *Server) apiCordon(w http.ResponseWriter, r *http.Request) {
	m, ok := s.maintOrUnavailable(w)
	if !ok {
		return
	}
	st, err := m.Cordon(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindMaintenance, wire.NewMaintenanceView(st.Cordoned, st.InFlight))
}

func (s *Server) apiUncordon(w http.ResponseWriter, r *http.Request) {
	m, ok := s.maintOrUnavailable(w)
	if !ok {
		return
	}
	st, err := m.Uncordon(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindMaintenance, wire.NewMaintenanceView(st.Cordoned, st.InFlight))
}

func (s *Server) apiReadiness(w http.ResponseWriter, r *http.Request) {
	m, ok := s.maintOrUnavailable(w)
	if !ok {
		return
	}
	st, err := m.State(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindMaintenance, wire.NewMaintenanceView(st.Cordoned, st.InFlight))
}

// apiInterrupted lists the jobs SweepInterrupted retired to interrupted, newest
// first — the post-reboot view an operator re-drives from. It reads the store
// directly and needs no maintenance seam: an interrupted row is terminal
// history, readable whether or not this surface owns the queue.
func (s *Server) apiInterrupted(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListInterruptedJobs(r.Context(), 0)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindJobList, wire.NewJobList(rows))
}
