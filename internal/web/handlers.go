package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// queueGlance is how many rows the /jobs and /workflows list views show. A
// homelab runs a handful at a time; a longer history is the events and
// per-job views. The dashboard's queue card no longer uses this — it counts
// the active queue only (jobSummary), never a capped page of history.
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

	row, err := s.authenticateTokenGated(r, token, s.now())
	if err != nil {
		if errors.Is(err, ErrKDFBusy) {
			// A flood is holding every KDF slot; this login ran no argon2id.
			// Tell the operator to retry rather than run the KDF unbounded.
			w.Header().Set("Retry-After", "1")
			s.render(w, r, http.StatusServiceUnavailable,
				loginPage("The server is busy verifying other sign-ins. Try again in a moment.", next, s.fingerprint, s.insecure))
			return
		}
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

	approvals, err := s.store.ListApprovals(r.Context(), true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pending, err := s.pendingApprovalsFrom(r, approvals)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Approvals = approvalSummaryFrom(pending)

	jobs, err := s.jobSummary(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Jobs = jobs

	ws, err := s.workspaceSummary(r, pending)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Workspaces = ws

	sources, err := s.sourceSummary(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Sources = sources

	checks, err := s.checkSummary(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Checks = checks

	summary, err := s.scheduleSummary(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Schedules = summary

	// A count only: reactive rules are CLI-managed and read-only on the web.
	if rules, err := s.rules(r); err == nil {
		vm.Rules = len(rules)
	}

	// A count only: ListIncompleteWorkflows returns bare IDs, so the card
	// never pays for GetWorkflow's per-step ledger just to size a number.
	inflight, err := s.store.ListIncompleteWorkflows(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Workflows = workflowSummary{InFlight: len(inflight)}

	s.render(w, r, http.StatusOK, dashboard(vm))
}

// handleSystem renders the control plane's view of itself: version + Litestream
// RPO and backup freshness (moved off the dashboard and the powerline).
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	ident, _ := identityOf(r.Context())
	vm := systemVM{Page: s.pageFor(r, "System", "system"), Operator: canWrite(ident)}
	vm.Self = s.selfObs(r.Context())
	if v, err := s.store.GetSetting(r.Context(), store.SettingPublicURL); err == nil {
		vm.PublicURL = v
	}

	backups, err := s.backups(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Backups = backups

	s.render(w, r, http.StatusOK, systemPage(vm))
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	rows, err := s.sources(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ident, _ := identityOf(r.Context())
	vm := sourcesVM{
		Page: s.pageFor(r, "Sources", "sources"), Sources: rows,
		Operator: canWrite(ident), SealAvailable: s.sealer != nil,
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "sources-add", Label: "source"}
	}
	s.render(w, r, http.StatusOK, sourcesList(vm))
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.schedules(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ident, _ := identityOf(r.Context())
	vm := schedulesVM{
		Page: s.pageFor(r, "Schedules", "schedules"), Schedules: rows,
		Operator: canWrite(ident), Prefill: s.schedulePrefillFor(r),
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "schedules-add", Label: "schedule"}
	}
	s.render(w, r, http.StatusOK, schedulesList(vm))
}

func (s *Server) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	rows, err := s.recentWorkflows(r, queueGlance)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm := workflowsVM{Page: s.pageFor(r, "Workflows", "workflows"), Workflows: rows}
	s.render(w, r, http.StatusOK, workflowsList(vm))
}

func (s *Server) handleWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	steps, err := s.store.GetWorkflow(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		vm := workflowVM{Page: s.pageFor(r, "Workflow", "workflows"), ID: id, NotFound: true}
		s.render(w, r, http.StatusNotFound, workflowDetail(vm))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	row := workflowRowFrom(id, steps)
	vm := workflowVM{
		Page:  s.pageFor(r, "Workflow", "workflows"),
		ID:    id,
		State: row.State,
		Steps: row.Steps,
	}
	s.render(w, r, http.StatusOK, workflowDetail(vm))
}

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	rows, err := s.policies(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ident, _ := identityOf(r.Context())
	vm := policiesVM{
		Page: s.pageFor(r, "Plan policies", "policies"), Policies: rows,
		Operator: canWrite(ident),
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "policies-add", Label: "policy"}
	}
	s.render(w, r, http.StatusOK, policiesList(vm))
}

func (s *Server) handleRunners(w http.ResponseWriter, r *http.Request) {
	rows, err := s.runners(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ident, _ := identityOf(r.Context())
	vm := runnersVM{
		Page: s.pageFor(r, "Runners", "runners"), Runners: rows,
		Operator: canWrite(ident),
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "runners-add", Label: "runner"}
	}
	s.render(w, r, http.StatusOK, runnersList(vm))
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	rows, err := s.secrets(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ident, _ := identityOf(r.Context())
	vm := secretsVM{
		Page: s.pageFor(r, "Secrets", "secrets"), Secrets: rows,
		Operator: canWrite(ident), SealAvailable: s.sealer != nil,
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "secrets-add", Label: "secret"}
	}
	s.render(w, r, http.StatusOK, secretsList(vm))
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
	ident, _ := identityOf(r.Context())
	source := job.Args.Source
	if source == "" {
		// An old row that predates Args.Source: fall back to the reverse
		// Dir→source lookup so "Run again" still resolves.
		source, _ = s.sourceForDir(r.Context(), job.Args.Dir)
	}
	vm := jobDetailVM{
		Page: s.pageFor(r, "Job #"+itoa64(id), "jobs"), Job: s.jobRow(job, s.now()),
		Operator: canWrite(ident), Source: source, CanSchedule: canScheduleJob(job),
		Rerunnable: len(job.Args.Argv) > 0,
	}
	if rule, ok := strings.CutPrefix(job.Runbook, "rule:"); ok {
		vm.FiredRule = rule
		ev, err := s.store.EventByJobID(r.Context(), job.ID)
		if err == nil {
			vm.TriggerEvent = &triggerEvent{
				ID: ev.ID, Source: ev.Source, Name: wire.EventName(ev), Time: clock(ev.ReceivedAt),
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			// Only a missing row is benign — a manually enqueued job that
			// happens to carry a rule runbook shows the rule line, no event.
			s.fail(w, r, err)
			return
		}
	}
	if vm.Rerunnable {
		vm.Rerun = jobRerunVM{
			Check:      hasFlag(job.Args.Argv, "--check"),
			Diff:       hasFlag(job.Args.Argv, "--diff"),
			Privileged: job.Args.Privileged,
			Limit:      flagValue(job.Args.Argv, "--limit"),
			Tags:       flagValue(job.Args.Argv, "--tags"),
			Command:    joinArgv(job.Args.Argv),
			Inventory:  job.Args.Inventory,
			ExtraVars:  extraVarLines(job.Args.ExtraVars),
		}
	}
	for _, e := range events {
		vm.Events = append(vm.Events, eventLine{Seq: e.Seq, Ts: clock(e.Ts), Level: e.Level, Msg: e.Msg})
	}
	if len(vm.Events) > 0 {
		vm.LastSeq = vm.Events[len(vm.Events)-1].Seq
	}
	s.render(w, r, http.StatusOK, jobDetail(vm))
}

// handleApprovals renders the pending-approvals list. The fingerprint is
// computed from the same ListApprovals rows the page renders, projected to the
// same wire payload the stream diffs, so the script's ?fp= can tell the stream
// "this state is already on screen" and skip a reload-only snapshot (F4). The
// page rows come from that same one fetch (pendingApprovalsFrom), so the fp is
// provably the state on screen — two fetches would leave a window where a
// change between them made the fp lie.
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	rows, err := s.listApprovals(r.Context(), true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm := dashboardVM{Page: s.pageFor(r, "Approvals", "approvals"), ApprovalFP: approvalsFP(rows)}
	pending, err := s.pendingApprovalsFrom(r, rows)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.NeedsYou = pending
	s.render(w, r, http.StatusOK, approvalsList(vm))
}

// handleApprovalStream live-updates the /approvals list page via SSE, the
// browser twin of apiApprovalStream: the same flusher assertion, then
// tailApprovals, which re-resolves the session identity (epoch pin included)
// each poll and caps lifetime at streamMaxLifetime. tailApprovals sets the SSE
// headers itself and enforces the shared stream cap.
func (s *Server) handleApprovalStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s.tailApprovals(w, r, flusher, false)
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

	// Whoever asked for the gated job may not release it, whatever their scope:
	// the gate exists to put a second principal in the path.
	self, err := s.decidedBySelf(r.Context(), approval.JobID, ident)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if self {
		http.Error(w, "this token asked for the gated job; it may not decide its own approval",
			http.StatusForbidden)
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
	source := r.URL.Query().Get("source")
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		limit = v
	}
	if limit < 100 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	events, err := s.store.ListEvents(r.Context(), store.EventFilter{Source: source, Limit: limit})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm := eventsVM{Page: s.pageFor(r, "Events", "events"), Source: source, Limit: limit, HasMore: len(events) == limit}
	for _, e := range events {
		vm.Events = append(vm.Events, eventRow{
			ID: e.ID, Source: e.Source, Received: clock(e.ReceivedAt),
			Name: wire.EventName(e), Outcome: e.Outcome, Reason: e.Reason, JobID: e.JobID,
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
			Name: wire.EventName(ex.Event), Outcome: ex.Event.Outcome, Reason: ex.Event.Reason,
			JobID: ex.Event.JobID,
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
		ID: j.ID, Runbook: j.Runbook, Kind: j.Kind, Trigger: j.Trigger, Source: "-",
		State: j.State, Age: jobAge(j, now), ExitCode: "-", Argv: j.Args.Argv,
	}
	if j.Args.Source != "" {
		row.Source = j.Args.Source
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
