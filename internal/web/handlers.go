package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// queueGlance is how many jobs the dashboard and jobs list show. A homelab
// runs a handful at a time; a longer history is the events and per-job views.
const queueGlance = 50

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	// An already-authenticated browser has no business on the login form.
	if _, err := s.resolve(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := sanitizeNext(r.URL.Query().Get("next"))
	s.render(w, r, http.StatusOK, loginPage("", next, s.fingerprint, s.insecure))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	token := r.PostFormValue("token")
	next := sanitizeNext(r.PostFormValue("next"))

	row, err := s.authenticateToken(r.Context(), token, s.now())
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrMalformedToken) {
			s.render(w, r, http.StatusUnauthorized,
				loginPage("That token is not valid, is expired, or was revoked.", next, s.fingerprint, s.insecure))
			return
		}
		s.log.Error("web: login", "err", err)
		http.Error(w, "authentication error", http.StatusInternalServerError)
		return
	}

	value, expiry := s.mintSession(row, s.now())
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		Secure:   !s.insecure,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   !s.insecure,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	vm := dashboardVM{Page: s.pageFor(r, "Dashboard", "dashboard")}

	banner, err := s.banner(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Banner = banner

	pending, err := s.pendingApprovals(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.NeedsYou = pending

	jobs, err := s.store.ListJobs(r.Context(), store.JobFilter{Limit: queueGlance})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := s.now()
	for _, j := range jobs {
		vm.Queue = append(vm.Queue, s.jobRow(j, now))
	}

	backups, err := s.backups(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Backups = backups

	ws, err := s.workspaces(r, pending)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Workspaces = ws

	s.render(w, r, http.StatusOK, dashboard(vm))
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListJobs(r.Context(), store.JobFilter{Limit: queueGlance})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm := dashboardVM{Page: s.pageFor(r, "Jobs", "jobs")}
	now := s.now()
	// Newest first reads better as a history than the store's oldest-first.
	for i := len(jobs) - 1; i >= 0; i-- {
		vm.Queue = append(vm.Queue, s.jobRow(jobs[i], now))
	}
	s.render(w, r, http.StatusOK, jobsList(vm))
}

func (s *Server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
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
	events, err := s.store.ListJobEvents(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm := jobDetailVM{Page: s.pageFor(r, "Job #"+itoa64(id), "jobs"), Job: s.jobRow(job, s.now())}
	for _, e := range events {
		vm.Events = append(vm.Events, eventLine{Seq: e.Seq, Ts: clock(e.Ts), Level: e.Level, Msg: e.Msg})
	}
	s.render(w, r, http.StatusOK, jobDetail(vm))
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	pending, err := s.pendingApprovals(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm := dashboardVM{Page: s.pageFor(r, "Approvals", "approvals"), NeedsYou: pending}
	s.render(w, r, http.StatusOK, approvalsList(vm))
}

func (s *Server) handleApprovalSurface(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	vm, err := s.approvalVM(r, id, "")
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, approvalSurface(vm))
}

// handleDecide is the phone Approve/Reject. The session authorizes it; the
// engine's Decide is still the one fail-closed path, so the web surface cannot
// approve a stale or expired plan a link could not. The approval token is
// derived here, not carried by the browser: a session is not a capability link.
func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ident, _ := identityOf(r.Context())
	if !mayApprove(ident) {
		http.Error(w, "this token may not decide approvals", http.StatusForbidden)
		return
	}
	if s.decider == nil {
		http.Error(w, "approvals cannot be decided from this surface", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	decision := r.PostFormValue("decision")
	if decision != reactive.DecisionApprove && decision != reactive.DecisionReject {
		http.Error(w, "decision must be approve or reject", http.StatusBadRequest)
		return
	}

	approval, err := s.store.GetApproval(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	token, _, err := reactive.ApprovalToken(s.deriver, approval.JobID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	result, derr := s.decider.Decide(r.Context(), id, decision, token, "web:"+ident.token.Name)
	if derr != nil && !errors.Is(derr, reactive.ErrStalePlan) {
		s.log.Error("web: decide", "approval", id, "err", derr)
		s.fail(w, r, derr)
		return
	}

	// Re-render the surface with the outcome: a decided or stale link is a
	// terminal notice on the same screen, never an error page.
	vm, err := s.approvalVM(r, id, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Decided = !errors.Is(derr, reactive.ErrStalePlan)
	vm.Outcome = result.Outcome
	vm.Notice = result.Message
	if errors.Is(derr, reactive.ErrStalePlan) {
		vm.Stale = true
		vm.StaleNotice = result.Message
		vm.Decided = false
	}
	s.render(w, r, http.StatusOK, approvalSurface(vm))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListEvents(r.Context(), store.EventFilter{Limit: 100})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm := eventsVM{Page: s.pageFor(r, "Events", "events")}
	for _, e := range events {
		vm.Events = append(vm.Events, eventRow{
			ID: e.ID, Source: e.Source, Received: clock(e.ReceivedAt),
			Name: eventName(e), Outcome: e.Outcome, Reason: e.Reason, JobID: e.JobID,
		})
	}
	s.render(w, r, http.StatusOK, eventsList(vm))
}

func (s *Server) handleEventTrace(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ex, err := reactive.Explain(r.Context(), s.store, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm := traceVM{
		Page: s.pageFor(r, "Event #"+itoa64(id), "events"),
		Event: eventRow{
			ID: ex.Event.ID, Source: ex.Event.Source, Received: clock(ex.Event.ReceivedAt),
			Name: eventName(ex.Event), Outcome: ex.Event.Outcome, Reason: ex.Event.Reason,
		},
	}
	for _, c := range ex.Trace {
		vm.Steps = append(vm.Steps, traceStep{Passed: c.Passed, Step: c.Step, Detail: c.Detail, Reason: c.Reason})
	}
	s.render(w, r, http.StatusOK, eventTrace(vm))
}

// ---- shared builders ----

func (s *Server) jobRow(j store.Job, now time.Time) jobRow {
	row := jobRow{
		ID: j.ID, Runbook: j.Runbook, Kind: j.Kind, Trigger: j.Trigger,
		State: j.State, Age: jobAge(j, now), ExitCode: "-",
	}
	if j.ExitCode != nil {
		row.ExitCode = strconv.Itoa(*j.ExitCode)
	}
	return row
}

func jobAge(j store.Job, now time.Time) string {
	switch {
	case j.State == store.StateQueued || j.State == store.StateAwaitingApproval:
		return humanAge(now.Sub(j.QueuedAt))
	case j.StartedAt != nil && j.FinishedAt != nil:
		return humanAge(j.FinishedAt.Sub(*j.StartedAt))
	case j.StartedAt != nil:
		return humanAge(now.Sub(*j.StartedAt))
	default:
		return humanAge(now.Sub(j.QueuedAt))
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("web: handler", "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// sanitizeNext keeps a post-login redirect on this origin: a path beginning
// with a single slash. Anything else (a scheme, a //host) falls back to the
// dashboard, so the login form cannot be turned into an open redirect.
func sanitizeNext(next string) string {
	if len(next) == 0 || next[0] != '/' || (len(next) > 1 && next[1] == '/') {
		return "/"
	}
	return next
}

func clock(t time.Time) string { return t.Format("2006-01-02 15:04:05Z") }
