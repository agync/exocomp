package web

import (
	"context"
	"errors"
	"net/http"
	"time"

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
// plan/apply sit behind (agent-interface §4). Reload and restart carry the same
// operator floor; readiness, the interrupted listing, and restart status are
// reads (canRead). None of this goes through the job queue: a cordon is daemon
// control, not a run, so there is no Spec to enqueue — the "queue/service
// pattern where applicable" does not apply to a control-plane toggle.

// Maintenance is the running daemon's drain-control seam. The concrete
// implementation lives in the daemon process (it pauses the scheduler and closes
// the queue's claim gate); the web surface only drives it. State also reports how
// many of the daemon's jobs are still in flight, which is what makes the
// readiness answer meaningful. Reload rebuilds the scheduler's registrations
// from the database through the same path SIGHUP and the reload interval use.
// Restart drains the daemon and re-execs the on-disk binary in place, and
// RestartStatus is the read a `restart --wait` polls.
type Maintenance interface {
	Cordon(ctx context.Context) (MaintenanceState, error)
	Uncordon(ctx context.Context) (MaintenanceState, error)
	State(ctx context.Context) (MaintenanceState, error)
	Reload(ctx context.Context) (ReloadState, error)
	Restart(ctx context.Context, force bool) (RestartState, error)
	RestartStatus(ctx context.Context) (RestartState, error)
}

// MaintenanceState is a point-in-time drain reading: whether the daemon is
// cordoned and how many jobs it still owns in the running state.
type MaintenanceState struct {
	Cordoned bool
	InFlight int
}

// ReloadState is what a schedule reload refreshed: how many enabled schedules
// the daemon now holds.
type ReloadState struct {
	Schedules int
}

// RestartState is a point-in-time restart reading: whether a drain-then-re-exec
// is pending, how many jobs it is still waiting on, and the deadline past which
// the daemon aborts (or, with --force, cancels the rest and restarts).
type RestartState struct {
	Pending  bool
	InFlight int
	Deadline time.Time
}

// ErrNoScheduler is the reload refusal when the web surface started before the
// daemon wired its scheduler handle: there is nothing to reload yet.
var ErrNoScheduler = errors.New("daemon scheduler is not running yet; retry once serve finishes starting")

// ErrNothingToRestart is the no-op refusal: the on-disk binary is byte-for-byte
// the image this process is already running, so a re-exec would change nothing.
var ErrNothingToRestart = errors.New("nothing to restart: the on-disk binary is unchanged from the running image")

// registerMaintenanceAPI mounts the /api/v1/maintenance routes. Cordon/uncordon
// carry the operator write floor; the reads carry the readonly floor.
func (s *Server) registerMaintenanceAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/maintenance/cordon",
		s.apiGuard(bearerOnly(canWrite), writeDeny("cordon the daemon", "operator"), s.apiCordon))
	mux.HandleFunc("POST /api/v1/maintenance/uncordon",
		s.apiGuard(bearerOnly(canWrite), writeDeny("uncordon the daemon", "operator"), s.apiUncordon))
	mux.HandleFunc("POST /api/v1/maintenance/reload",
		s.apiGuard(bearerOnly(canWrite), writeDeny("reload the daemon", "operator"), s.apiReload))
	mux.HandleFunc("POST /api/v1/maintenance/restart",
		s.apiGuard(bearerOnly(canWrite), writeDeny("restart the daemon", "operator"), s.apiRestart))
	mux.HandleFunc("GET /api/v1/maintenance/restart", s.apiAuth(s.apiRestartStatus))
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

// apiReload rebuilds the daemon's schedule registrations from the database
// through the seam — the same path SIGHUP and the reload interval drive — and
// answers how many enabled schedules the daemon now holds.
func (s *Server) apiReload(w http.ResponseWriter, r *http.Request) {
	m, ok := s.maintOrUnavailable(w)
	if !ok {
		return
	}
	st, err := m.Reload(r.Context())
	if err != nil {
		if errors.Is(err, ErrNoScheduler) {
			s.apiError(w, http.StatusServiceUnavailable, wire.NewError(wire.CodeInternal, err.Error()))
			return
		}
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindMaintenanceReload, wire.NewReloadView(st.Schedules))
}

// apiRestart asks the daemon to restart after draining and answers the pending
// state. The request is a one-way signal: the seam cordons and returns at once,
// and a background drain-then-re-exec follows. --force (the ?force=1 query)
// cancels the remaining jobs instead of aborting on timeout. A no-op (the
// on-disk binary is unchanged) is refused as a conflict so a script branches on
// it rather than reading a restart that never happens.
func (s *Server) apiRestart(w http.ResponseWriter, r *http.Request) {
	m, ok := s.maintOrUnavailable(w)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	// The restart is control-plane state, not a queue job, so the daemon log is
	// its audit record: who asked, and whether they forced it.
	if id, ok := identityOf(r.Context()); ok {
		s.log.Info("api: restart requested", "token", id.token.Name, "force", force)
	}
	st, err := m.Restart(r.Context(), force)
	if err != nil {
		if errors.Is(err, ErrNothingToRestart) {
			s.apiError(w, http.StatusConflict, wire.NewError(wire.CodeConflict, err.Error()))
			return
		}
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindMaintenanceRestart, wire.NewRestartView(st.Pending, st.InFlight, st.Deadline))
}

// apiRestartStatus reports a pending restart's drain progress, the read a
// `restart --wait` polls. It is a read (canRead), so a readonly token may watch
// a restart an operator requested.
func (s *Server) apiRestartStatus(w http.ResponseWriter, r *http.Request) {
	m, ok := s.maintOrUnavailable(w)
	if !ok {
		return
	}
	st, err := m.RestartStatus(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindMaintenanceRestart, wire.NewRestartView(st.Pending, st.InFlight, st.Deadline))
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

// apiInterrupted lists the jobs ReconcileNonTerminal retired to interrupted,
// newest first — the post-reboot view an operator re-drives from. It reads
// the store directly and needs no maintenance seam: an interrupted row is
// terminal history, readable whether or not this surface owns the queue.
func (s *Server) apiInterrupted(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListInterruptedJobs(r.Context(), 0)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindJobList, wire.NewJobList(rows))
}
