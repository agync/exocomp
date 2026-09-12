package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/deadman"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/version"
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
		http.Redirect(w, r, s.url("/"), http.StatusSeeOther)
		return
	}
	next := s.sanitizeNext(r.URL.Query().Get("next"))
	s.render(w, r, http.StatusOK, loginPage("", next, s.fingerprint, s.insecure, s.base))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	token := r.PostFormValue("token")
	next := s.sanitizeNext(r.PostFormValue("next"))

	row, err := s.authenticateTokenGated(r, token, s.now())
	if err != nil {
		if errors.Is(err, ErrKDFBusy) {
			// A flood is holding every KDF slot; this login ran no argon2id.
			// Tell the operator to retry rather than run the KDF unbounded.
			w.Header().Set("Retry-After", "1")
			s.render(w, r, http.StatusServiceUnavailable,
				loginPage("The server is busy verifying other sign-ins. Try again in a moment.", next, s.fingerprint, s.insecure, s.base))
			return
		}
		if errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrMalformedToken) {
			s.render(w, r, http.StatusUnauthorized,
				loginPage("That token is not valid, is expired, or was revoked.", next, s.fingerprint, s.insecure, s.base))
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
	http.Redirect(w, r, s.url("/login"), http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	vm := dashboardVM{Page: s.pageFor(r, "Dashboard", "dashboard")}
	// The app bar's page-own fact: the host and how long the control plane has
	// been up — identity stays in the powerline (W7).
	if host := hostname(); host != "" {
		vm.Page.Meta = host + " · " + s.uptimeLabel()
	} else {
		vm.Page.Meta = s.uptimeLabel()
	}

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

// handleSystem renders the control plane's self-care surface (SPEC §0): the
// identity strip (build stamp + host), the four leg cards, and the powerline
// of per-leg anchors. The public-URL Change verb posts to the landed L-B3
// write (system_write.go); this handler only marks up its value.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	vm, err := s.systemVMFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, systemPage(vm))
}

// systemVMFor builds the full /system surface, dead-man leg included. It is
// split out of handleSystem so the Arm dialog's JS-off refusal re-render can
// rebuild the same page holding the refused form (deadman_write.go).
func (s *Server) systemVMFor(r *http.Request) (systemVM, error) {
	ident, _ := identityOf(r.Context())
	vm := systemVM{Page: s.pageFor(r, "System", "system"), Operator: canWrite(ident)}
	vm.Page.Meta = "is the control plane itself OK"
	vm.Self = s.selfObs(r.Context())
	vm.Litestream = s.litestreamVM(r.Context())
	if v, err := s.store.GetSetting(r.Context(), store.SettingPublicURL); err == nil {
		vm.PublicURL = v
	}
	vm.BasePath = s.basePath()

	backups, err := s.backups(r)
	if err != nil {
		return systemVM{}, err
	}
	vm.Backups = backups

	vm.Version = version.Version
	vm.Commit = version.Commit
	vm.Platform = platformLabel()
	vm.Host = hostname()
	vm.Uptime = s.uptimeLabel()
	vm.BackupsWord, vm.BackupsState = backupsPowerWord(backups)
	vm.Ntfy = s.ntfyCardVM(r.Context())

	vm.Deadman = s.deadmanVM(r)
	// The self-care powerline's dead-man segment takes the card's chip colour
	// (SPEC §0.1) without recomputing the age: the word stays the ONE-rule age
	// signal() read, the colour now carries armed/none/tripped too.
	vm.Page.Signal.DeadManState = vm.Deadman.StateClass
	vm.Doctor = s.doctorMeta(r.Context())
	return vm, nil
}

// handleDeadman renders the /system/deadman leg page: the same dead-man card
// full-width under the page chrome, with the Arm dialog and Disarm verb.
func (s *Server) handleDeadman(w http.ResponseWriter, r *http.Request) {
	vm, err := s.deadmanVMFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, deadmanPage(vm))
}

func (s *Server) deadmanVMFor(r *http.Request) (viewDeadmanVM, error) {
	vm := viewDeadmanVM{Page: s.pageFor(r, "Dead-man", "system")}
	vm.Page.Meta = "last good backup age vs the trip threshold"
	vm.Deadman = s.deadmanVM(r)
	return vm, nil
}

// deadmanVM reads the dead-man leg's render state. The age it quotes is the
// store's ONE rule (LastSucceededBackupTime) — the same read the powerline's
// vm.Page.Signal.DeadMan makes, never a second computation. Everything else is
// enrichment: the threshold and the sealed heartbeat's presence, the markers
// the daemon's trip monitor writes, and the target/snapshot facts that name
// the good backup without re-deriving its age.
func (s *Server) deadmanVM(r *http.Request) deadmanVM {
	ctx := r.Context()
	now := s.now()
	dm := deadmanVM{
		State: "none", StateClass: "wait", StateWord: "NONE",
		Figure: "—", FigureClass: "dim", LastGood: "never",
		Heartbeat: "not wired — no external heartbeat yet",
	}

	// The ONE age rule, read once.
	when, ever := s.store.LastSucceededBackupTime(ctx)

	if run, target, ok, err := s.store.NewestSucceededBackupRun(ctx); ok {
		finished := run.FinishedAt
		if finished == nil {
			finished = &run.StartedAt
		}
		dm.LastGood = deadman.CountdownAge(now.Sub(*finished)) + " ago · " + target.Name +
			" #" + deadmanShortID(run.SnapshotID)
	} else if err != nil {
		s.log.Error("web: deadman", "err", err.Error())
	}

	threshold, terr := s.store.GetSetting(ctx, store.SettingDeadmanThreshold)
	threshold = strings.TrimSpace(threshold)
	if terr == nil && threshold != "" {
		dm.Threshold = threshold
		parsed, perr := time.ParseDuration(threshold)
		switch {
		case !ever || perr != nil:
			// Armed with nothing ever succeeded — or a threshold that will not
			// parse — is unbounded age: TRIPPED, fail-closed.
			dm.State = "tripped"
			dm.StateClass = "fail"
			dm.StateWord = "TRIPPED"
			dm.Figure = "—"
			dm.FigureClass = "fail"
			dm.FigureMeta = "since a backup succeeded · threshold " + threshold
		case now.Sub(when) < parsed:
			dm.State = "armed"
			dm.StateClass = "ok"
			dm.StateWord = "ARMED"
			dm.Figure = deadman.CountdownAge(parsed - now.Sub(when))
			dm.FigureClass = ""
			dm.FigureMeta = "until it trips"
		default:
			dm.State = "tripped"
			dm.StateClass = "fail"
			dm.StateWord = "TRIPPED"
			dm.Figure = deadman.CountdownAge(now.Sub(when))
			dm.FigureClass = "fail"
			dm.FigureMeta = "since a backup succeeded · threshold " + threshold
		}
	}

	// The heartbeat fact is presence, never a value: sealed heartbeats read
	// wired, and the ping clock comes from the marker the monitor stamps.
	hbWired := false
	if _, err := s.store.GetSecret(ctx, deadman.SecretHeartbeatURL); err == nil {
		hbWired = true
	}
	if dm.State == "none" {
		dm.FigureMeta = "no threshold · nothing trips if backups stop"
		if !hbWired {
			dm.FigureMeta = "no threshold, no heartbeat · nothing trips if backups stop"
		}
	}
	if hbWired {
		pingedAgo := "never"
		if pinged, perr := s.store.GetSetting(ctx, store.SettingDeadmanPingedAt); perr == nil {
			if when, ok := msTime(pinged); ok {
				pingedAgo = deadman.CountdownAge(now.Sub(when)) + " ago"
			}
		}
		if dm.State == "tripped" {
			dm.Heartbeat = "last ping " + pingedAgo + " · the external service is paging"
			dm.HeartbeatClass = "fail"
		} else {
			dm.Heartbeat = "wired · sealed deadman/heartbeat_url · pinged " + pingedAgo
		}
	}

	switch dm.State {
	case "tripped":
		if paged, perr := s.store.GetSetting(ctx, store.SettingDeadmanPagedAt); perr == nil {
			if when, ok := msTime(paged); ok {
				dm.Paged = "ntfy · once · " + deadman.CountdownAge(now.Sub(when)) + " ago"
			}
		}
		dm.NewestRuns = s.deadmanNewestRuns(r)
		targets, err := s.store.ListBackupTargets(ctx, false)
		if err == nil && len(targets) > 0 {
			dm.QueueTarget = targets[0].Name
		}
		dm.Sentence = "Clears itself the moment any target's run SUCCEEDS. A failed or still-running newer run does not mask the age."
	case "armed":
		dm.Sentence = "At zero: the heartbeat goes silent and the external service pages you; ntfy pages once; the dashboard band and doctor read TRIPPED until a backup succeeds."
	}

	ident, _ := identityOf(r.Context())
	dm.Operator = canWrite(ident)
	return dm
}

// deadmanNewestRuns is the TRIPPED card's per-target fact: each target's newest
// run with its state (or disabled), the same newest-run read the backups rows
// make — capped at three so the fact stays one line.
func (s *Server) deadmanNewestRuns(r *http.Request) string {
	ctx := r.Context()
	now := s.now()
	targets, err := s.store.ListBackupTargets(ctx, true)
	if err != nil {
		return ""
	}
	var parts []string
	for _, t := range targets {
		if len(parts) == 3 {
			break
		}
		if !t.Enabled {
			parts = append(parts, t.Name+" disabled")
			continue
		}
		runs, err := s.store.ListBackupRuns(ctx, t.ID, 1)
		if err != nil || len(runs) == 0 {
			parts = append(parts, t.Name+" never ran")
			continue
		}
		run := runs[0]
		when := run.StartedAt
		if run.FinishedAt != nil {
			when = *run.FinishedAt
		}
		parts = append(parts, t.Name+" "+run.State+" "+deadman.CountdownAge(now.Sub(when))+" ago")
	}
	return strings.Join(parts, " · ")
}

// deadmanRefusal re-renders the page the Arm submit came from at 422 with the
// dialog's refusal state re-opened (JS-off gets the same bytes a JS re-render
// would swap in).
func (s *Server) deadmanRefusal(w http.ResponseWriter, r *http.Request, from string, form *deadmanFormVM) {
	form.Open = true
	if from == "/system/deadman" {
		vm, err := s.deadmanVMFor(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		vm.Deadman.Form = form
		s.render(w, r, http.StatusUnprocessableEntity, deadmanPage(vm))
		return
	}
	vm, err := s.systemVMFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.Deadman.Form = form
	s.render(w, r, http.StatusUnprocessableEntity, systemPage(vm))
}

// msTime parses a stored unix-ms timestamp; ok=false on garbage.
func msTime(raw string) (time.Time, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(n).UTC(), true
}

// deadmanShortID renders a snapshot id at the length a card fact scans (the
// same truncation the CLI's backup list uses, at the shorter display size the
// fixture draws).
func deadmanShortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// handleLitestream renders the /system/litestream leg surface (SPEC §3.2): the
// full-width card plus the ▾ views — the generated config (read-only yml,
// secrets by name) and the deep link into the DR drawer's replica mode (the
// drawer itself is L-S4's). Read-only by design (ADR 0012): the daemon
// generates config and observes, it never runs litestream, so there is no
// pause verb on this surface.
func (s *Server) handleLitestream(w http.ResponseWriter, r *http.Request) {
	vm := litestreamPageVM{Page: s.pageFor(r, "System / Litestream", "system"), LS: s.litestreamVM(r.Context())}
	vm.Page.Meta = "the fast-RPO leg · read-only · no pause verb"
	s.render(w, r, http.StatusOK, litestreamPage(vm))
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	vm, err := s.sourcesVMFor(r, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, sourcesList(vm))
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	vm, err := s.schedulesVMFor(r, nil, nil)
	if err != nil {
		s.fail(w, r, err)
		return
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
	vm, err := s.policiesVMFor(r, nil, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, policiesList(vm))
}

func (s *Server) handleRunners(w http.ResponseWriter, r *http.Request) {
	vm, err := s.runnersVMFor(r, nil, "")
	if err != nil {
		s.fail(w, r, err)
		return
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
		// A2: /secrets?key=ntfy/token (m75's Seal-token href) focuses the row.
		FocusKey: r.URL.Query().Get("key"),
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
		vm.Queue = append(vm.Queue, s.jobRowForList(r.Context(), jobs[i], now))
	}
	// The count moves to the app bar (SPEC §2.3): the list's own count span is
	// dropped from the markup, and the app bar meta names the ordering.
	vm.Page.Meta = itoa(int64(len(vm.Queue))) + " · newest first"
	s.render(w, r, http.StatusOK, jobsList(vm))
}

// reasonEventWindow is how many tail events a list row reads for its reason
// line: the PLAY RECAP and the final error both sit at the very end of the
// stream (the recap is ansible's last stdout, the error band the last system
// event), so a bounded tail is enough and a full read is never paid for.
const reasonEventWindow = 200

// jobRowForList builds one jobs-list row: the shared row facts plus the
// D4 reason line (a bounded tail read for the reason-carrying states) and the
// TRIGGER cell. The rerun parent and the manual face are the only things that
// cost a full event read, and only on the rare rerun/manual rows.
func (s *Server) jobRowForList(ctx context.Context, j store.Job, now time.Time) jobRow {
	row := s.jobRow(j, now)
	if j.State == store.StateFailed || j.State == store.StateDegraded || j.State == store.StateTimedOut {
		if events, err := s.store.LastJobEvents(ctx, j.ID, reasonEventWindow); err == nil {
			row.Reason = jobReason(parseJobOutcome(events), j.State, row.ExitCode)
		}
	}
	switch j.Trigger {
	case rerunTrigger:
		row.TriggerLabel = "Rerun"
		row.TriggerMeta = "operator"
		if events, err := s.store.ListJobEvents(ctx, j.ID); err == nil {
			if parent := rerunOf(events); parent > 0 {
				row.TriggerMeta = "of #" + itoa64(parent) + " · operator"
			}
		}
	case "schedule":
		row.TriggerLabel = "Schedule"
		// The schedule association is inferred: a schedule-fired job carries
		// the schedule's name as its Runbook (the scheduler sets it for every
		// kind) and no schedule foreign key on the row. Look the name back up
		// for the cron time; a job whose schedule was since deleted keeps a
		// bare "schedule" qualifier rather than duplicating the playbook cell.
		row.TriggerMeta = scheduleMeta(ctx, s.store, j)
	case "manual":
		row.TriggerLabel = "Manual"
		row.TriggerMeta = "cli · operator"
		if events, err := s.store.ListJobEvents(ctx, j.ID); err == nil {
			if face := actorFace(events); face != "" {
				row.TriggerMeta = face + " · operator"
			}
		}
	default:
		row.TriggerLabel = orDash(j.Trigger)
	}
	return row
}

// scheduleMeta is a schedule-fired row's TRIGGER meta: "{name} · {fire time}",
// the fire time being the job's own queued instant (the scheduler enqueues at
// the fire). A deleted schedule falls back to a bare "schedule" qualifier.
func scheduleMeta(ctx context.Context, st *store.Store, j store.Job) string {
	if sc, err := st.GetSchedule(ctx, RunbookLabel(j.Runbook)); err == nil {
		return sc.Name + " · " + j.QueuedAt.UTC().Format("15:04")
	}
	return "schedule"
}

// actorFace reads the surface a token-attributed write recorded on the job's
// ledger (service.recordActor's face field); "" when no actor event exists, so
// a CLI-run job's caller can render its "cli" default.
func actorFace(events []store.JobEvent) string {
	for _, e := range events {
		if e.RawJSON == "" {
			continue
		}
		var payload struct {
			Face string `json:"face"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &payload); err != nil || payload.Face == "" {
			continue
		}
		return payload.Face
	}
	return ""
}

func (s *Server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	vm, err := s.jobDetailVMFor(w, r)
	if err != nil {
		return // jobDetailVMFor already wrote the failure response
	}
	s.render(w, r, http.StatusOK, jobDetail(vm))
}

// jobDetailVMFor builds the job detail page's render state: the run card, the
// event tail, the verdict band, and the schedule/rerun dialog prefills. A
// lookup or build failure writes the failure response and returns a non-nil
// error so the caller never double-renders. The schedule and rerun handlers
// share it for their 422 re-renders, which only override the dialog's form
// state on top.
func (s *Server) jobDetailVMFor(w http.ResponseWriter, r *http.Request) (jobDetailVM, error) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return jobDetailVM{}, errors.New("bad job id")
	}
	job, err := s.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return jobDetailVM{}, err
	}
	if err != nil {
		s.fail(w, r, err)
		return jobDetailVM{}, err
	}
	events, err := s.store.ListJobEvents(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return jobDetailVM{}, err
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
	// The Output meta's "ended {hh:mm:ss}" reads the terminal run's finished
	// clock; a live job keeps the streaming form instead.
	vm.EndedClock = ""
	if job.FinishedAt != nil {
		vm.EndedClock = logClock(*job.FinishedAt)
	}
	// J3/S6: three routes flash to /jobs/{id} and the layout renders the banner
	// for every page, but on the job's own page the fact is the dashed "rerun
	// of #N" chip — the banner is dropped here, at the render seam, not at the
	// call sites.
	vm.Page.Flash = ""
	// The page's own header (the old .pageheader with its Raw log / Rerun /
	// Schedule buttons) is gone: the phone app bar carries the back chevron,
	// the stacked title, and the compact Raw log / Rerun actions (SPEC §2.0).
	// Rerun opens the resubmit dialog via the JS-off ?rerun=1 path; Raw log is
	// the plain text tail in a new tab.
	vm.Page.BackHref = s.url("/jobs")
	vm.Page.TitleSub = vm.Job.Title
	// T1: the job detail page is a no-page-scroll flex column — the log box
	// owns the scroll and ends above the powerline (app.css .page-job).
	vm.Page.PageClass = "page-job"
	vm.Page.Actions = append(vm.Page.Actions, pageAction{
		Href: s.url("/jobs/" + itoa64(id) + "/raw"), Label: "Raw log", Kind: "control", Glyph: glyphDocument,
	})
	if vm.Operator && vm.Job.State.Terminal() && vm.Rerunnable {
		vm.Page.Actions = append(vm.Page.Actions, pageAction{
			Href: s.url("/jobs/" + itoa64(id) + "?rerun=1"), Label: "Rerun", Kind: "accent", Glyph: glyphRotate,
		})
	}

	facts := parseJobOutcome(events)
	vm.RerunFailedOn = facts.Unreachable

	// The "Queued by {actor} from the {face}" sentence reads the actor record
	// off the ledger; a CLI run records none, so it reads "operator from the
	// cli" (S2).
	actor, aerr := s.service().JobActor(r.Context(), job.ID)
	if aerr != nil {
		s.fail(w, r, aerr)
		return jobDetailVM{}, aerr
	}
	actorName, face := "operator", "cli"
	if actor.Kind != "" {
		actorName = actor.Name
		face = actor.Face
		if face == "" {
			face = "api"
		}
	}
	runner := job.Args.RunnerName
	if runner == "" {
		runner = hostname()
	}

	// The Ref tile shows the source's current sync (S1): its ref and short sha.
	var src *store.Source
	if source != "" {
		if row, serr := s.store.SourceByName(r.Context(), source); serr == nil {
			src = &row
			vm.RefLabel = refLabel(row)
			vm.Ref = row.Ref
		}
	}

	if vm.Rerunnable {
		vm.Rerun = jobRerunVM{
			Check:      hasFlag(job.Args.Argv, "--check"),
			Diff:       hasFlag(job.Args.Argv, "--diff"),
			Privileged: job.Args.Privileged,
			Tolerate:   job.Args.TolerateUnreachable,
			Limit:      flagValue(job.Args.Argv, "--limit"),
			Tags:       flagValue(job.Args.Argv, "--tags"),
			Command:    joinArgv(job.Args.Argv),
			Inventory:  job.Args.Inventory,
			ExtraVars:  extraVarLines(job.Args.ExtraVars),
		}
		// ?rerun=1 renders the dialog open (JS-off); ?limit= prefills the Limit
		// field, the target of the verdict band's "Rerun limited to …" verb.
		if r.URL.Query().Get("rerun") == "1" {
			vm.RerunOpen = true
			if lim := r.URL.Query().Get("limit"); lim != "" {
				vm.Rerun.Limit = lim
			}
		}
	}

	vm.RunCard = s.runCardVM(r.Context(), job, source, src, facts, actorName, face, runner, rerunOf(events))
	vm.Verdict = s.jobVerdict(job, facts, events)
	// A kind=doctor job's findings (SPEC §7 A1): the self-check's subsystem
	// entries as rows, parsed from the job's own structured events. Every
	// other kind renders no findings.
	if job.Kind == health.JobKind {
		vm.Findings = parseDoctorFindings(events)
	}

	rerunParent := rerunOf(events)
	for _, e := range events {
		line := eventLine{Seq: e.Seq, Ts: logClock(e.Ts), Level: e.Level, Msg: e.Msg}
		if e.Level == store.LevelSystem {
			line.Band, line.BandClass = bandFor(job, e.Msg, facts, rerunParent)
		}
		vm.Events = append(vm.Events, line)
	}
	if len(vm.Events) > 0 {
		vm.LastSeq = vm.Events[len(vm.Events)-1].Seq
	}
	// The last terminal band repeats completion with the rendered line count
	// (copy.txt): "▸ succeeded — exit 0 · N lines · log complete". The running
	// band is not terminal, so a live job's tail never reads complete.
	for i := len(vm.Events) - 1; i >= 0; i-- {
		if vm.Events[i].Band != "" && vm.Events[i].BandClass != "run" {
			vm.Events[i].Band += fmt.Sprintf(" · %d lines · log complete", len(vm.Events))
			break
		}
	}
	// The pin's initial data-state is server-set (§2): complete when the
	// terminal stream carries its verdict band, cut when the runner was lost
	// before it could write one, following on a live page.
	vm.PinState = "following"
	if job.State.Terminal() {
		vm.PinState = "complete"
		if !hasTerminalBand(vm.Events) {
			vm.PinState = "cut"
		}
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
			return jobDetailVM{}, err
		}
	}
	return vm, nil
}

// handleJobRaw serves a job's full event log as plain text — one message per
// line, no timestamps or level chrome — so an operator can copy the raw output
// into a terminal or log viewer. The events are already redacted on the way in
// (AGENTS.md invariant 4), so this is the same text the log box shows, minus
// the UI. It is the target of the job detail's "view raw log" button.
func (s *Server) handleJobRaw(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetJob(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, r, err)
		return
	}
	events, err := s.store.ListJobEvents(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Msg)
		b.WriteByte('\n')
	}
	_, _ = w.Write([]byte(b.String()))
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

// resolveEventsFilters maps the events URL params onto the canonical filter
// state: source is the switch's split ("" all · "checks" · "alerts", the store
// vocabulary's singular "check" accepted as an alias), outcome one of the six
// chips or "". The legacy ?filter= values keep working as aliases
// (filter=checks → source=checks, filter=fired → outcome=fired) so existing
// links survive; the explicit params win when both are present.
func resolveEventsFilters(source, outcome, filter string) (string, string) {
	source = normalizeSource(source)
	outcome = normalizeOutcome(outcome)
	if source == "" {
		source = filterSource(filter)
	}
	if outcome == "" {
		outcome = filterOutcome(filter)
	}
	return source, outcome
}

func normalizeSource(s string) string {
	switch s {
	case "check", "checks":
		return "checks"
	case "alerts":
		return "alerts"
	default:
		return ""
	}
}

func normalizeOutcome(o string) string {
	switch o {
	case "ignored", "matched", "suppressed", "gated", "fired", "error":
		return o
	default:
		return ""
	}
}

func filterSource(filter string) string {
	switch filter {
	case "checks", "check":
		return "checks"
	case "alerts":
		return "alerts"
	default:
		return ""
	}
}

func filterOutcome(filter string) string {
	switch filter {
	case "ignored", "matched", "suppressed", "gated", "fired", "error":
		return filter
	default:
		return ""
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.now()

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

	source, outcome := resolveEventsFilters(r.URL.Query().Get("source"), r.URL.Query().Get("outcome"), r.URL.Query().Get("filter"))
	ef := store.EventFilter{Limit: limit}
	switch source {
	case "checks":
		ef.Source = reactive.SourceCheck
	case "alerts":
		ef.NotSource = reactive.SourceCheck
	}
	if outcome != "" {
		ef.Outcomes = []store.EventOutcome{store.EventOutcome(outcome)}
	}

	events, err := s.store.ListEvents(ctx, ef)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	funnel, err := s.store.CountEventsByOutcomeSince(ctx, now.Add(-24*time.Hour))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ledger, err := s.store.CountEventsByOutcome(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	vm := eventsVM{
		Page:    s.pageFor(r, "Events", "events"),
		Funnel:  funnel,
		Source:  source,
		Outcome: outcome,
		Empty:   ledger.Total() == 0,
	}
	for _, e := range events {
		row := eventRow{
			ID: e.ID, Source: e.Source, Received: receivedClock(e.ReceivedAt, now),
			Rel:  humanAge(now.Sub(e.ReceivedAt)),
			Name: wire.EventName(e), Rule: e.MatchedRule, Outcome: e.Outcome,
			Reason: e.Reason, JobID: e.JobID, Normalized: prettyJSON(e.Normalized),
			Trace: decodeSteps(e.Trace),
		}
		row = s.enrichEventRow(ctx, row, e)
		vm.Rows = append(vm.Rows, row)
		vm.Chips.add(row)
	}
	vm.Page.Meta = "last 24h"
	s.render(w, r, http.StatusOK, eventsList(vm))
}

// enrichEventRow turns the stored one-line reason into the reason column's
// copy (SPEC §1a): FIRED and GATED override the emission-time sentence with
// live facts — the fired reason reads "job #{id} · {state} · exit {n}", a
// gated event reads "awaiting approval" and links to the approval; a
// suppressed cooldown or duplicate maps to "cooldown {d} left" /
// "duplicate of #{id}"; ignored reads "no rule matched" and a matched event
// names its rule ("rule {name} · not yet run"). Anything without a mapping
// keeps the stored reason.
func (s *Server) enrichEventRow(ctx context.Context, row eventRow, e store.Event) eventRow {
	switch e.Outcome {
	case store.OutcomeFired:
		if e.JobID != nil {
			if job, err := s.store.GetJob(ctx, *e.JobID); err == nil {
				row.Reason = fmt.Sprintf("job #%d · %s · exit %s", job.ID, job.State, exitOf(job))
				row.ReasonLink = s.url("/jobs/" + itoa64(job.ID))
			}
		}
	case store.OutcomeGated:
		if e.JobID != nil {
			if approval, err := s.store.ApprovalByJob(ctx, *e.JobID); err == nil {
				row.Reason = "awaiting approval"
				row.ReasonLink = s.url("/approvals/" + itoa64(approval.ID))
			}
		}
	case store.OutcomeSuppressed:
		row.Reason = enrichSuppressed(e.Reason)
	case store.OutcomeIgnored:
		row.Reason = "no rule matched"
	case store.OutcomeMatched:
		if e.MatchedRule != "" {
			row.Reason = "rule " + e.MatchedRule + " · not yet run"
		}
	}
	return row
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
	// A check event carries its hysteresis ledger on the row (embedded at
	// emission); alert events carry none. It renders the "WHY THIS COUNTED"
	// card. The cold-start body needs the daemon's start time.
	if ex.Event.Source == "check" && ex.Event.CheckObservations != "" && ex.Event.CheckObservations != "[]" {
		var obs []struct {
			ObservedAt  string `json:"observed_at"`
			Status      string `json:"status"`
			Consecutive int    `json:"consecutive"`
		}
		if err := json.Unmarshal([]byte(ex.Event.CheckObservations), &obs); err == nil {
			for _, o := range obs {
				vm.CheckObservations = append(vm.CheckObservations, checkObservationRow{
					ObservedAt: o.ObservedAt, Status: o.Status, Consecutive: o.Consecutive,
				})
			}
		}
	}
	if !s.startedAt.IsZero() {
		vm.StartedAt = s.startedAt.UTC().Format("15:04")
	}
	s.render(w, r, http.StatusOK, eventTrace(vm))
}

// ---- shared builders ----

func (s *Server) jobRow(j store.Job, now time.Time) jobRow {
	row := jobRow{
		ID: j.ID, Runbook: j.Runbook, Kind: j.Kind, Trigger: j.Trigger, Source: "-",
		State: j.State, Age: jobAge(j, now), ExitCode: "-", Argv: j.Args.Argv,
		Title: jobTitle(j), CheckMode: hasFlag(j.Args.Argv, "--check"),
		Duration: jobDuration(j, now), FinishedRel: jobFinishedRel(j, now),
		FinishedClock: jobFinishedClock(j),
	}
	if j.Args.Source != "" {
		row.Source = j.Args.Source
	}
	if j.ExitCode != nil {
		row.ExitCode = strconv.Itoa(*j.ExitCode)
	}
	var parts []string
	if row.Source != "-" && row.Source != "" {
		parts = append(parts, row.Source)
	}
	if row.CheckMode {
		parts = append(parts, "check mode")
	}
	row.SourceMeta = strings.Join(parts, " · ")
	return row
}

// jobDuration is the RAN column's primary line: the finished run's wall time,
// a running job's time so far, else how long the row has queued.
func jobDuration(j store.Job, now time.Time) string {
	if j.StartedAt != nil && j.FinishedAt != nil {
		return runDuration(j.FinishedAt.Sub(*j.StartedAt))
	}
	if j.StartedAt != nil {
		return runDuration(now.Sub(*j.StartedAt))
	}
	return humanAge(now.Sub(j.QueuedAt))
}

// jobFinishedRel is the RAN column's meta line: "16:31 today" for a finished
// run, empty while it still runs.
func jobFinishedRel(j store.Job, now time.Time) string {
	if j.FinishedAt != nil {
		return finishedRel(*j.FinishedAt, now)
	}
	return ""
}

// jobFinishedClock is the finished clock alone (HH:MM), the phone RAN meta's
// "{HH:MM} · time" split from the finishedRel string (SPEC §3); empty while
// the run still runs.
func jobFinishedClock(j store.Job) string {
	if j.FinishedAt != nil {
		return j.FinishedAt.UTC().Format("15:04")
	}
	return ""
}

// rerunOf reads a rerun job's parent id from the record the rerun handler
// appended to its ledger (jobs_rerun.go); 0 when the run is not a rerun or the
// record is missing.
func rerunOf(events []store.JobEvent) int64 {
	for _, e := range events {
		var payload struct {
			Of int64 `json:"rerun_of"`
		}
		if e.RawJSON == "" {
			continue
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &payload); err != nil || payload.Of <= 0 {
			continue
		}
		return payload.Of
	}
	return 0
}

// refLabel renders the Ref tile's "{ref} @ {short sha}" from a source's current
// sync (S1) — an approximation, never the run's own pinned commit.
func refLabel(src store.Source) string {
	ref, sha := src.Ref, shortSHA(src.LastSyncSHA)
	switch {
	case ref != "" && sha != "":
		return ref + " @ " + sha
	case ref != "":
		return ref
	default:
		return sha
	}
}

// shortSHA renders a sync sha at the length a human scans, mirroring
// sourceRow.Short for a store row.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// runCardVM builds the run card: the chip row, the "Queued by ..." sentence,
// the verdict number, and the RUN CONFIGURATION tile grid. source is the
// resolved source name (Args.Source or the reverse Dir lookup).
func (s *Server) runCardVM(ctx context.Context, job store.Job, source string, src *store.Source,
	facts jobOutcomeFacts, actorName, face, runner string, rerunOfID int64) jobRunCardVM {
	rc := jobRunCardVM{
		State: job.State, Tool: job.Kind, CheckMode: hasFlag(job.Args.Argv, "--check"),
		RerunOf: rerunOfID, Title: jobTitle(job), JobID: job.ID,
		RanLine: ranLine(job), Tiles: s.runCardTiles(ctx, source, job, src),
	}
	rc.Sentence = "Queued by " + actorName + " from the " + face + " · " +
		utcStamp(job.QueuedAt) + " · ran on " + runner
	if job.ExitCode != nil {
		rc.Verdict = "exit " + strconv.Itoa(*job.ExitCode)
		if job.State == store.StateDegraded {
			rc.Tolerated = true
		}
	}
	if job.State == store.StateDegraded && facts.RecapFound {
		rc.DegradedChip = hostCountLine(len(facts.Unreachable)) + " · " +
			strings.Join(facts.Unreachable, ", ")
	}
	return rc
}

// runCardTiles builds the RUN CONFIGURATION tile grid. The Tolerate tile is the
// cause tile on a degraded run: it wears the outcome's inset state ring. source
// is the resolved source name (Args.Source or the reverse Dir lookup).
func (s *Server) runCardTiles(ctx context.Context, source string, job store.Job, src *store.Source) []configTile {
	var tiles []configTile
	argv := job.Args.Argv
	if source != "" {
		tile := configTile{Label: "Source", Value: source + " ↗"}
		if src != nil {
			tile.Link = s.url("/sources/" + src.Name)
		}
		tiles = append(tiles, tile)
	}
	if src != nil {
		if val := refLabel(*src); val != "" {
			tiles = append(tiles, configTile{Label: "Ref", Value: val, Link: s.url("/sources/" + src.Name)})
		}
	}
	if job.Args.Inventory != "" {
		tiles = append(tiles, configTile{Label: "Inventory", Value: job.Args.Inventory})
	}
	if limit := flagValue(argv, "--limit"); limit != "" {
		tiles = append(tiles, configTile{Label: "Limit", Value: limit})
	}
	if tags := flagValue(argv, "--tags"); tags != "" {
		tiles = append(tiles, configTile{Label: "Tags", Value: tags})
	}
	mode := ""
	if job.Kind == executor.KindTofu && len(argv) > 1 {
		mode = argv[1] // plan|apply
	} else {
		var parts []string
		if hasFlag(argv, "--check") {
			parts = append(parts, "check")
		}
		if hasFlag(argv, "--diff") {
			parts = append(parts, "diff")
		}
		mode = strings.Join(parts, " + ")
	}
	if mode != "" {
		tiles = append(tiles, configTile{Label: "Mode", Value: mode})
	}
	var priv []string
	if job.Args.Privileged {
		priv = append(priv, "unsandboxed")
	}
	if hasFlag(argv, "--become") {
		priv = append(priv, "become")
	}
	if hasFlag(argv, "--sudo") {
		priv = append(priv, "sudo")
	}
	if len(priv) > 0 {
		tiles = append(tiles, configTile{Label: "Privileged", Value: strings.Join(priv, " · ")})
	}
	if job.Args.RunSecretBinding != "" {
		val := job.Args.RunSecretBinding
		if pairs, err := s.store.RunSecretsByBinding(ctx, job.Args.RunSecretBinding); err == nil && len(pairs) > 1 {
			val += " · " + itoa(int64(len(pairs)-1)) + " more"
		}
		tiles = append(tiles, configTile{Label: "Secrets bound", Value: val})
	}
	tol := configTile{Label: "Tolerate unreachable", Value: "off"}
	if job.Args.TolerateUnreachable {
		tol = configTile{Label: "Tolerate unreachable", Value: "on · settles degraded", Wait: true}
	}
	if job.State == store.StateDegraded {
		tol.Cause = true
	}
	tiles = append(tiles, tol)
	return tiles
}

// jobVerdict builds the verdict band — populated only on FAILED and DEGRADED.
func (s *Server) jobVerdict(job store.Job, facts jobOutcomeFacts, events []store.JobEvent) *jobVerdictVM {
	if job.State != store.StateFailed && job.State != store.StateDegraded {
		return nil
	}
	v := jobVerdictVM{Class: "fail", BandClass: "failed"}
	code := "-"
	if job.ExitCode != nil {
		code = strconv.Itoa(*job.ExitCode)
	}
	if job.State == store.StateDegraded {
		v.Class = "wait"
		v.BandClass = "degraded"
		if facts.RecapFound {
			v.Keyword = fmt.Sprintf("DEGRADED — SUCCEEDED WITH WARNINGS · %d OF %d HOSTS UNREACHABLE",
				len(facts.Unreachable), len(facts.Hosts))
		} else {
			v.Keyword = "DEGRADED — SUCCEEDED WITH WARNINGS"
		}
	} else if facts.RecapFound {
		v.Keyword = fmt.Sprintf("FAILED · EXIT %s — %d OF %d HOSTS UNREACHABLE",
			code, len(facts.Unreachable), len(facts.Hosts))
	} else {
		v.Keyword = "FAILED · EXIT " + code
	}
	v.Sentence = verdictSentence(job, facts)

	for _, h := range facts.Hosts {
		row := jobVerdictHost{Host: h.Host}
		switch {
		case h.Unreachable > 0:
			row.Result = "UNREACHABLE"
			row.Class = v.Class
			reason := unreachableReason(events, h.Host)
			if job.State == store.StateDegraded {
				if reason == "" {
					reason = "tolerated"
				} else {
					reason = "tolerated · " + reason
				}
			}
			row.Recap = reason
		case h.Failed > 0:
			row.Result = "FAILED"
			row.Class = "fail"
			row.Recap = h.RawCounts
			if row.Recap == "" {
				row.Recap = "failed"
			}
		default:
			row.Result = "OK"
			row.Class = "ok"
			row.Recap = h.RawCounts
			if hasFlag(job.Args.Argv, "--check") && h.Changed > 0 {
				row.Recap += fmt.Sprintf(" · %d tasks would change", h.Changed)
			}
		}
		v.Hosts = append(v.Hosts, row)
	}

	// Verbs (invariant 3) — only when the run actually has unreachable hosts to
	// act on: the 1a rerun-tolerating verb, or the 1b checks/limit pair.
	if len(facts.Unreachable) > 0 {
		if job.State == store.StateDegraded {
			hosts := strings.Join(facts.Unreachable, ", ")
			downSince, found := unreachableSince(events, facts.Unreachable)
			down := "unknown"
			if found {
				down = downSince.Format("15:04")
			}
			v.Verb = "Open checks — " + hosts + " DOWN since " + down + " →"
			v.VerbHref = s.url("/checks")
			v.VerbSecond = "Rerun limited to " + hosts + " →"
			v.VerbSecondHref = s.url("/jobs/" + itoa64(job.ID) + "?rerun=1&limit=" +
				url.QueryEscape(strings.Join(facts.Unreachable, ",")))
		} else {
			v.Verb = "Rerun tolerating unreachable hosts →"
			v.VerbHref = s.url("/jobs/" + itoa64(job.ID) + "?rerun=1")
			v.VerbMeta = "settles DEGRADED when the reachable hosts succeed · " +
				"same as exocomp job rerun " + itoa64(job.ID) + " --tolerate-unreachable"
		}
	}
	return &v
}

// verdictSentence is the verdict band's one line: it names what happened to
// the reachable and unreachable hosts, in plain words.
func verdictSentence(job store.Job, facts jobOutcomeFacts) string {
	var reach []string
	for _, h := range facts.Hosts {
		if h.Unreachable == 0 && h.Failed == 0 {
			reach = append(reach, h.Host)
		}
	}
	if job.State == store.StateDegraded {
		if len(reach) == 0 {
			return "No host was reachable; " + andList(facts.Unreachable) +
				" were skipped as unreachable because this run tolerates offline hosts."
		}
		return "Every play on " + andList(reach) + " completed; " + andList(facts.Unreachable) +
			" were skipped as unreachable because this run tolerates offline hosts. They have not been configured."
	}
	if len(facts.Failed) > 0 && len(facts.Unreachable) == 0 {
		return "The play failed on " + andList(facts.Failed) + "."
	}
	if len(reach) == 0 {
		return "Ansible could not reach " + andList(facts.Unreachable) + "."
	}
	return "Ansible could not reach " + andList(facts.Unreachable) + "; every play on " +
		andList(reach) + " completed."
}

// unreachableReason extracts the stderr text after ansible's
// "fatal: [host]: UNREACHABLE! =>" marker, the per-host why (N2).
func unreachableReason(events []store.JobEvent, host string) string {
	needle := "fatal: [" + host + "]: UNREACHABLE!"
	for _, e := range events {
		idx := strings.Index(e.Msg, needle)
		if idx < 0 {
			continue
		}
		rest := e.Msg[idx+len(needle):]
		if i := strings.Index(rest, "=>"); i >= 0 {
			rest = strings.TrimSpace(rest[i+2:])
		}
		if rest != "" {
			return rest
		}
	}
	return ""
}

// unreachableSince finds the latest "fatal: [host]: UNREACHABLE!" event among
// the named hosts — the last time ansible reported them unreachable, the
// approximation the degraded band's "DOWN since {time}" reads. found is false
// when no fatal line named any of the hosts.
func unreachableSince(events []store.JobEvent, hosts []string) (latest time.Time, found bool) {
	for _, e := range events {
		for _, h := range hosts {
			if strings.Contains(e.Msg, "fatal: ["+h+"]: UNREACHABLE!") && e.Ts.After(latest) {
				latest = e.Ts
				found = true
			}
		}
	}
	return latest, found
}

// bandFor re-renders one of the run's state bands with the copy strings: the
// queue writes "running …", "succeeded (exit N)", "failed (exit N)" and
// "degraded (exit N, …)", and the output card shows the load-bearing copy in
// their place. Everything else is a plain system line. rerunOfID is the parent
// the child's ledger recorded (0 for a run that is not a rerun).
func bandFor(job store.Job, msg string, facts jobOutcomeFacts, rerunOfID int64) (string, string) {
	switch {
	case strings.HasPrefix(msg, "running UNSANDBOXED"):
		return "", ""
	case strings.HasPrefix(msg, "running "):
		// Copy names a rerun's running band as "rerun of #{id} · tolerate
		// unreachable on · workspace lock {ws} acquired". The workspace-lock
		// fact has no source in the job data (the wave-1 queue writes the
		// running line, not the lock acquisition), so it is omitted rather
		// than fabricated; the rerun and tolerance facts compose from the
		// child's ledger. A non-rerun keeps the queue's own running line.
		if rerunOfID > 0 {
			line := "▸ running — rerun of #" + itoa64(rerunOfID)
			if job.Args.TolerateUnreachable {
				line += " · tolerate unreachable on"
			}
			return line, "run"
		}
		return "▸ running — " + msg[len("running "):], "run"
	case strings.HasPrefix(msg, "succeeded (exit "):
		// The succeeded verdict band is the ok-tint twin of the failed one;
		// the VM appends the "· N lines · log complete" completion tail.
		return "▸ succeeded — exit " + exitOf(job), "ok"
	case strings.HasPrefix(msg, "failed (exit "):
		return "▸ failed — exit " + exitOf(job), "fail"
	case strings.HasPrefix(msg, "degraded (exit "):
		return fmt.Sprintf("▸ degraded — exit %s tolerated · %d hosts unreachable (%s) · reachable hosts succeeded",
			exitOf(job), len(facts.Unreachable), strings.Join(facts.Unreachable, ", ")), "wait"
	default:
		return "", ""
	}
}

// exitOf renders a job's recorded exit code, or "-" when none was recorded.
func exitOf(job store.Job) string {
	if job.ExitCode == nil {
		return "-"
	}
	return strconv.Itoa(*job.ExitCode)
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
// home page (the base path under a prefix, "/" at the root), so the login form
// cannot be turned into an open redirect. A valid path that is not yet
// prefixed is prefixed, so a bare deep link still lands inside the surface;
// urlWithBase leaves an already-prefixed path alone, so next never double
// prefixes.
func (s *Server) sanitizeNext(next string) string {
	if len(next) == 0 || next[0] != '/' || (len(next) > 1 && next[1] == '/') {
		if s.base == "" {
			return "/"
		}
		return s.base
	}
	return s.url(next)
}

func clock(t time.Time) string { return t.Format("2006-01-02 15:04:05Z") }

// logClock renders a clock column's HH:MM:SS (R9): the date lives on the card,
// so the log's fixed 60px column carries the seconds alone.
func logClock(t time.Time) string { return t.UTC().Format("15:04:05") }
