package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/version"
	"exocomp.dev/exocomp/internal/wire"
)

// This file mounts the read-only operator HTTP/JSON API (agent-interface §3.4)
// on the SAME ADR-0009 listener as the browser surface. Every route reuses the
// browser's auth resolution (s.resolve, the same session/bearer + KDF-gate path
// requireAuth uses) and marshals the SAME wire projections the CLI's
// `--output json` emits, so a resource's API bytes and its CLI bytes
// are identical. It is strictly additive and read-only: no route enqueues,
// mutates, or shells out, and no POST/PUT/DELETE is registered.
//
// GET /api/v1/health calls internal/health.Probe — the same probe definition
// `doctor --output json` calls — so the two summaries can never diverge. It is
// served from a short-lived cache (healthCacheTTL): the probe forks a
// subprocess per optional tool and opens the secrets envelope, and neither
// belongs on a per-request path any token can drive.
//
// Deferred, not built here: every write endpoint.

// codeInvalidArgument is the taxonomy code for a malformed request parameter (a
// non-numeric id, an unknown filter value). The wire taxonomy has no store
// sentinel for it — it is a boundary condition — so it is named here and carried
// through wire.NewError like the other boundary codes (unauthenticated/forbidden).
const codeInvalidArgument = "invalid_argument"

// registerAPI mounts the /api/v1 GET routes on mux. It is called from Handler()
// before securityHeaders wraps the mux, so these routes carry the same headers
// as the browser surface. A write method on any of these paths falls through to
// the mux's own 405 (Go 1.22 method-aware routing), which is what the gate
// asserts.
func (s *Server) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/whoami", s.apiAuth(s.apiWhoami))
	mux.HandleFunc("GET /api/v1/jobs", s.apiAuth(s.apiJobs))
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.apiAuth(s.apiJobGet))
	mux.HandleFunc("GET /api/v1/jobs/{id}/events", s.apiAuth(s.apiJobEvents))
	mux.HandleFunc("GET /api/v1/jobs/{id}/stream", s.apiAuth(s.apiJobStream))
	mux.HandleFunc("GET /api/v1/workspaces", s.apiAuth(s.apiWorkspaces))
	mux.HandleFunc("GET /api/v1/sources", s.apiAuth(s.apiSources))
	mux.HandleFunc("GET /api/v1/approvals", s.apiAuth(s.apiApprovals))
	// A literal segment takes precedence over the {id} wildcard at the same
	// position (net/http ServeMux's own precedence rule), so this can neither
	// be shadowed by nor shadow /api/v1/approvals/{id}.
	mux.HandleFunc("GET /api/v1/approvals/stream", s.apiAuth(s.apiApprovalStream))
	mux.HandleFunc("GET /api/v1/approvals/{id}", s.apiAuth(s.apiApprovalGet))
	// The plan diff carries resource values, so it sits behind the plan-view
	// floor (mayApprove: operator or approve), NOT the readonly floor the rest
	// of the reads use — the same scope the browser approval surface renders it
	// behind.
	mux.HandleFunc("GET /api/v1/approvals/{id}/plan",
		s.apiGuard(mayApprove, "this token's scope may not read plan diffs", s.apiApprovalPlan))
	mux.HandleFunc("GET /api/v1/schedules", s.apiAuth(s.apiSchedules))
	mux.HandleFunc("GET /api/v1/netgroups", s.apiAuth(s.apiNetgroups))
	mux.HandleFunc("GET /api/v1/checks", s.apiAuth(s.apiChecks))
	mux.HandleFunc("GET /api/v1/rules", s.apiAuth(s.apiRules))
	mux.HandleFunc("GET /api/v1/silences", s.apiAuth(s.apiSilences))
	mux.HandleFunc("GET /api/v1/mutes", s.apiAuth(s.apiMutes))
	mux.HandleFunc("GET /api/v1/events", s.apiAuth(s.apiEvents))
	// A literal segment takes precedence over the {id} wildcard at the same
	// position, so this can neither be shadowed by nor shadow
	// /api/v1/events/{id}.
	mux.HandleFunc("GET /api/v1/events/summary", s.apiAuth(s.apiEventsSummary))
	mux.HandleFunc("GET /api/v1/events/{id}", s.apiAuth(s.apiEventGet))
	mux.HandleFunc("GET /api/v1/secrets", s.apiAuth(s.apiSecrets))
	mux.HandleFunc("GET /api/v1/runners", s.apiAuth(s.apiRunners))
	mux.HandleFunc("GET /api/v1/runners/{name}", s.apiAuth(s.apiRunnerGet))
	mux.HandleFunc("GET /api/v1/backups", s.apiAuth(s.apiBackups))
	mux.HandleFunc("GET /api/v1/health", s.apiAuth(s.apiHealth))
	s.registerWriteAPI(mux)
	s.registerPushAPI(mux)
	s.registerMaintenanceAPI(mux)
}

// apiAuth wraps a read JSON handler with the browser surface's own auth
// resolution and the readonly-or-higher floor. It is apiGuard specialized to
// canRead: every valid token may read.
func (s *Server) apiAuth(next http.HandlerFunc) http.HandlerFunc {
	return s.apiGuard(canRead, "this token's scope may not read", next)
}

// apiGuard wraps a JSON handler with the browser surface's own auth resolution
// and a scope predicate. It is NOT a fork of the auth: it calls the same
// s.resolve requireAuth calls, with the same KDF-gate and TouchToken behaviour.
// It differs from requireAuth only in how it renders the boundary — a machine
// caller wants a wire error envelope and a status code, never the browser's
// 303-to-login or plain-text "unauthorized". The scope predicate rejects an
// out-of-scope token with forbidden BEFORE any store read or token touch is
// issued (agent-interface §4), so a write route never touches state for a
// caller it will refuse.
func (s *Server) apiGuard(allow func(identity) bool, denyMsg string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := s.resolve(r)
		if err != nil {
			switch {
			case errors.Is(err, ErrKDFBusy):
				w.Header().Set("Retry-After", "1")
				s.apiError(w, http.StatusServiceUnavailable,
					wire.NewError(wire.CodeKDFBusy, err.Error()))
			case errors.Is(err, ErrAuthThrottled):
				// Same "busy, retry later" code the taxonomy already carries
				// (exit 7), with the status that names WHY: too many failures.
				w.Header().Set("Retry-After", strconv.Itoa(int(authFailBase.Seconds())))
				s.apiError(w, http.StatusTooManyRequests,
					wire.NewError(wire.CodeKDFBusy, err.Error()))
			case errors.Is(err, ErrUnauthenticated):
				w.Header().Set("WWW-Authenticate", `Bearer realm="exocomp"`)
				s.apiError(w, http.StatusUnauthorized,
					wire.NewError(wire.CodeUnauthenticated, err.Error()))
			default:
				s.log.Error("api: authenticate", "err", err)
				s.apiError(w, http.StatusInternalServerError,
					wire.NewError(wire.CodeInternal, "authentication error"))
			}
			return
		}
		if !allow(id) {
			s.apiError(w, http.StatusForbidden,
				wire.NewError(wire.CodeForbidden, denyMsg))
			return
		}
		if err := s.store.TouchToken(r.Context(), id.token.ID); err != nil {
			s.log.Warn("api: touch token", "token", id.token.Name, "err", err)
		}
		next(w, r.WithContext(withIdentity(r.Context(), id)))
	}
}

// canRead reports whether a resolved scope may read. All three ADR-0009 scopes
// are readonly-or-higher, so any valid token qualifies; the switch is the
// explicit floor, and rejects an unknown/empty scope rather than defaulting open.
func canRead(id identity) bool {
	switch id.token.Scope {
	case store.ScopeReadonly, store.ScopeOperator, store.ScopeApprove:
		return true
	default:
		return false
	}
}

// canWrite reports whether a resolved scope may enqueue a mutating job. Only
// the operator scope may; readonly and the approve-only scope may not. It is the
// W floor for plan/apply/run (agent-interface §4).
func canWrite(id identity) bool {
	return id.token.Scope == store.ScopeOperator
}

// canApprove reports whether a resolved scope may decide an approval on the
// MACHINE face. It is the approve scope alone: ADR 0014 §3 maps readonly/
// operator/approve onto R/W/A, and a token that may enqueue a gated apply must
// not also hold the release for the gate it raised. The browser
// surface keeps mayApprove's operator-or-approve floor — that is the human at
// the console, not a credential handed to an agent — and both paths additionally
// refuse a principal deciding its own write (decidedBySelf).
func canApprove(id identity) bool { return id.token.Scope == store.ScopeApprove }

// apiSuccess writes a 200 wire success envelope. It goes through the exact
// wire.WriteSuccess the CLI uses, so the bytes match `--output json` for the
// same kind and projection.
func (s *Server) apiSuccess(w http.ResponseWriter, kind string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := wire.WriteSuccess(w, kind, data); err != nil {
		s.log.Error("api: write success", "kind", kind, "err", err)
	}
}

// apiError writes a wire error envelope with an explicit status.
func (s *Server) apiError(w http.ResponseWriter, status int, e wire.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = wire.WriteErrorValue(w, e)
}

// apiFail maps a store/reactive error onto the taxonomy and the right HTTP
// status, then writes it as a wire error envelope. A not_found becomes 404, a
// conflict 409, and so on; anything unmatched is a 500 internal.
func (s *Server) apiFail(w http.ResponseWriter, err error) {
	e := wire.ErrorFor(err)
	s.apiError(w, httpStatusForCode(e.Code), e)
}

// httpStatusForCode maps a wire taxonomy code onto its HTTP status. It is the
// HTTP analogue of wire.ExitForCode: one table, no per-handler guessing.
func httpStatusForCode(code string) int {
	switch code {
	case wire.CodeNotFound:
		return http.StatusNotFound
	case wire.CodeUnauthenticated:
		return http.StatusUnauthorized
	case wire.CodeForbidden:
		return http.StatusForbidden
	case wire.CodeKDFBusy:
		return http.StatusServiceUnavailable
	case wire.CodeStreamsOverCap:
		return http.StatusServiceUnavailable
	case codeInvalidArgument:
		return http.StatusBadRequest
	case wire.CodeConflict, wire.CodeConcurrencyHeld, wire.CodeStalePlan, wire.CodeDuplicate:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// badRequest writes a 400 with the invalid-argument code and a caller message.
func (s *Server) badRequest(w http.ResponseWriter, msg string) {
	s.apiError(w, http.StatusBadRequest, wire.NewError(codeInvalidArgument, msg))
}

// --- whoami -----------------------------------------------------------------

// kindWhoami names the identity-reflection payload.
const kindWhoami = "whoami"

// whoamiView reflects the calling token back to itself: enough for a phone
// companion to show who it is signed in as and how much scope it holds, never
// any secret material (the token string authenticated the request; it is not
// echoed). Server carries the daemon's own identity so the Connect screen can
// show "exocompd <version> · <hostname>" without fishing the host out of the URL.
type whoamiView struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	Via       string     `json:"via,omitempty"`
	ExpiresAt *string    `json:"expires_at,omitempty"`
	Server    serverView `json:"server"`
}

// serverView is the daemon's own identity: the build version (internal/version,
// the one source the rest of the app reports), the host it runs on, and when
// the process came up. All are omitempty so a build with no version stamp, a
// host that cannot report its name, or a zero start time renders an honest
// empty field rather than a placeholder.
type serverView struct {
	Version  string `json:"version,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// StartedAt is the daemon's start time — the exact "since daemon start"
	// anchor the events screen's cold-start label needs.
	StartedAt *string `json:"started_at,omitempty"`
}

// apiWhoami returns the calling token's own identity. Any authenticated token
// may call it: it reflects only the caller the guard already resolved, reads no
// other row, and reveals nothing a token does not already know about itself.
func (s *Server) apiWhoami(w http.ResponseWriter, r *http.Request) {
	id, ok := identityOf(r.Context())
	if !ok {
		// apiAuth put the identity on the context before dispatching here; its
		// absence is a wiring fault, not a caller error.
		s.apiError(w, http.StatusInternalServerError, wire.NewError(wire.CodeInternal, "no resolved identity"))
		return
	}
	view := whoamiView{
		ID: id.token.ID, Name: id.token.Name, Scope: id.token.Scope, Via: id.via,
		Server: serverView{Version: version.Version, Hostname: hostname()},
	}
	if !s.startedAt.IsZero() {
		started := s.startedAt.UTC().Format(time.RFC3339)
		view.Server.StartedAt = &started
	}
	if id.token.ExpiresAt != nil {
		exp := id.token.ExpiresAt.UTC().Format(time.RFC3339)
		view.ExpiresAt = &exp
	}
	s.apiSuccess(w, kindWhoami, view)
}

// hostname reports the host the daemon runs on, or the empty string when the OS
// cannot answer — a degraded identity line, never a failed request.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// --- jobs -------------------------------------------------------------------

// apiJobs lists jobs with the same filters the CLI's `jobs list` supports
// (JobFilter: state, runbook, concurrency_key, limit). JobFilter has no cursor
// field, so cursor paging is not offered here — adding it would be a store
// change, out of scope for this read-only API.
func (s *Server) apiJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.JobFilter{
		Runbook:        q.Get("runbook"),
		ConcurrencyKey: q.Get("concurrency_key"),
	}
	for _, st := range q["state"] {
		if !knownJobState(st) {
			s.badRequest(w, "state "+strconv.Quote(st)+" is not a job state")
			return
		}
		filter.States = append(filter.States, store.JobState(st))
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.badRequest(w, "limit must be a non-negative integer")
			return
		}
		filter.Limit = n
	}

	rows, err := s.store.ListJobs(r.Context(), filter)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindJobList, wire.NewJobViewsResolved(r.Context(), s.store, rows))
}

// apiJobGet returns one job. A well-formed but unknown id is 404. Plan is
// populated from the job's own events (the same terminal plan/check event
// executor.TofuPlanChanges/AnsibleCheckChanges read locally) so a remote
// `workspace plan` can reproduce the local exit-2-on-changes decision without
// the raw driver JSON, which never crosses this seam. A failure listing
// events fails the whole request rather than silently omitting Plan, so a
// caller never mistakes a store error for "no changes".
func (s *Server) apiJobGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "job id must be a positive integer")
		return
	}
	job, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	events, err := s.store.ListJobEvents(r.Context(), id)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	view := wire.NewJobViewResolved(r.Context(), s.store, job)
	view.Plan = wire.NewPlanOutcomeView(events)
	view.Progress = wire.ExtractJobProgress(job, events)
	s.apiSuccess(w, wire.KindJob, view)
}

// apiJobEvents returns a job's append-only, already-redacted event log, exactly
// as `jobs events` projects it. An optional ?since=<seq> returns only events
// after that sequence, so an agent can tail without re-reading; it filters the
// store rows before projecting, so the bytes stay from the same marshaller.
// Like the CLI, a missing job yields an empty list rather than a 404.
func (s *Server) apiJobEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "job id must be a positive integer")
		return
	}
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.badRequest(w, "since must be an integer sequence")
			return
		}
		since = n
	}
	rows, err := s.store.ListJobEvents(r.Context(), id)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if since > 0 {
		kept := rows[:0]
		for _, e := range rows {
			if e.Seq > since {
				kept = append(kept, e)
			}
		}
		rows = kept
	}
	s.apiSuccess(w, wire.KindJobEvents, wire.NewJobEventList(rows))
}

// apiJobStream tails one job's events as SSE, gated by apiAuth instead of the
// browser's requireAuth. It reuses tailJobEvents — the same append-only
// job_event tail, monotonic Seq cursor, and done-on-terminal ending the
// browser surface streams — so the two surfaces cannot diverge; only
// existence and error rendering differ here (a wire envelope, not the
// browser's 404/303). The shared stream cap's refusal also answers as a wire
// envelope (streams_over_cap + Retry-After), so `jobs follow --remote` treats
// an over-cap tail as the retryable busy it is instead of a dead stream.
func (s *Server) apiJobStream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "job id must be a positive integer")
		return
	}
	if _, err := s.store.GetJob(r.Context(), id); err != nil {
		s.apiFail(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.apiError(w, http.StatusInternalServerError,
			wire.NewError(wire.CodeInternal, "streaming unsupported"))
		return
	}
	s.tailJobEvents(w, r, id, flusher, true)
}

// --- workspaces / sources ---------------------------------------------------

func (s *Server) apiWorkspaces(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindWorkspaceList, wire.NewWorkspaceList(rows))
}

func (s *Server) apiSources(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListSources(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindSourceList, wire.NewSourceList(rows))
}

// --- approvals --------------------------------------------------------------

// apiApprovals lists approvals. Like `approval list`, it shows pending-only by
// default; ?all=true includes already-decided approvals.
func (s *Server) apiApprovals(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListApprovals(r.Context(), !queryTrue(r, "all"))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindApprovalList, wire.NewApprovalList(rows))
}

func (s *Server) apiApprovalGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "approval id must be a positive integer")
		return
	}
	a, err := s.store.GetApproval(r.Context(), id)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	view := wire.NewApprovalView(a)
	stale, err := s.approvalStaleView(r.Context(), a)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	view.Stale = stale
	prov, err := s.approvalProvenance(r.Context(), a)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	view.Provenance = prov
	s.apiSuccess(w, wire.KindApproval, view)
}

// approvalStaleView returns the structured stale-plan block for an approval, or
// nil when the workspace has not moved under the saved plan. It sources the
// movement from the store (store.ApprovalStaleness), never from the reactive
// engine's prose, so the approval.get view and the 409 stale_plan body carry
// the same facts.
func (s *Server) approvalStaleView(ctx context.Context, a store.Approval) (*wire.ApprovalStaleView, error) {
	move, stale, err := s.store.ApprovalStaleness(ctx, a)
	if err != nil {
		return nil, err
	}
	if !stale {
		return nil, nil
	}
	return wire.NewApprovalStaleView(move), nil
}

// approvalProvenance answers screen 02's "why you". An approval a reactive rule
// raised carries its alert, the rule that matched, and when; a manual or
// scheduled one carries only its origin. The alert/rule/matched_at come from the
// event ledger row that named the job (EventByJobID) — structured columns and
// the event's structured normalized_json — never from a free-text message.
func (s *Server) approvalProvenance(ctx context.Context, a store.Approval) (*wire.ApprovalProvenanceView, error) {
	ev, err := s.store.EventByJobID(ctx, a.JobID)
	if err == nil {
		return wire.NewApprovalProvenanceView(wire.OriginAlert, wire.EventName(ev), ev.MatchedRule, &ev.ReceivedAt), nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	// No event named the job: an operator asked for it by hand (CLI/API), or a
	// schedule fired it — the job's trigger tells which.
	origin := wire.OriginManual
	job, jerr := s.store.GetJob(ctx, a.JobID)
	switch {
	case jerr == nil:
		if job.Trigger == sched.Trigger {
			origin = wire.OriginSchedule
		}
	case errors.Is(jerr, store.ErrNotFound):
		// The job was pruned; the origin stays manual.
	default:
		return nil, jerr
	}
	return wire.NewApprovalProvenanceView(origin, "", "", nil), nil
}

// apiApprovalStream streams the pending-approvals list as SSE, gated by the
// same read scope as apiApprovals (apiAuth), so the phone companion can
// replace polling GET /api/v1/approvals with one open connection. It reuses
// tailApprovals — the same re-resolved-identity poll and lifetime cap
// tailJobEvents has — for the same reason apiJobStream reuses tailJobEvents:
// one implementation, so the two approvals surfaces cannot diverge.
func (s *Server) apiApprovalStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.apiError(w, http.StatusInternalServerError,
			wire.NewError(wire.CodeInternal, "streaming unsupported"))
		return
	}
	s.tailApprovals(w, r, flusher, true)
}

// apiApprovalPlan returns an approval's sealed plan as a structured, redacted
// diff — never the raw blob, which can carry secret-backed attribute values the
// web approval surface redacts. It opens the plan through the SAME decrypt path
// the browser renders through (PlanOpener.OpenSealedFile) and classifies it
// through the SAME shared core (classifyPlan, which renderPlan also uses), so a
// value the web shows redacted is redacted here too and the two surfaces cannot
// diverge. It is gated by the same plan-view scope (mayApprove). A surface with
// no opener is 503 and an approval with no plan is 404, matching what the web
// path shows in each case.
func (s *Server) apiApprovalPlan(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "approval id must be a positive integer")
		return
	}
	a, err := s.store.GetApproval(r.Context(), id)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if s.plans == nil {
		s.apiError(w, http.StatusServiceUnavailable, wire.NewError(wire.CodeInternal,
			"this surface holds no envelope; plan diffs cannot be opened here"))
		return
	}
	if a.PlanArtifact == nil {
		s.apiError(w, http.StatusNotFound, wire.NewError(wire.CodeNotFound, "this approval carries no plan"))
		return
	}
	art, err := s.store.GetArtifact(r.Context(), *a.PlanArtifact)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	blob, err := s.planShowBlob(r, art)
	if err != nil {
		s.log.Error("api: open sealed plan", "approval", id, "err", err)
		s.apiError(w, http.StatusInternalServerError, wire.NewError(wire.CodeInternal, "cannot open the sealed plan"))
		return
	}
	resources, ok := classifyPlan(blob)
	if !ok {
		s.log.Error("api: parse sealed plan", "approval", id)
		s.apiError(w, http.StatusInternalServerError, wire.NewError(wire.CodeInternal, "cannot parse the sealed plan"))
		return
	}
	rows := make([]wire.PlanResourceView, 0, len(resources))
	for _, rc := range resources {
		// classifyPlan keeps no-op/read rows for the plan-detail page; the
		// /api/v1 plan view drops them IN the bucket loop, before
		// NewApprovalPlanView, so they never land in `changes` or corrupt
		// LineCount.
		if rc.Action == "no-op" || rc.Action == "read" {
			continue
		}
		row := wire.PlanResourceView{Address: rc.Address, Action: rc.Action, Redacted: rc.Redacted}
		for _, at := range rc.Attrs {
			row.Attrs = append(row.Attrs, wire.PlanAttrView{Name: at.Name, Before: at.Before, Value: at.After, Redacted: at.Redacted})
		}
		rows = append(rows, row)
	}
	s.apiSuccess(w, wire.KindApprovalPlan, wire.NewApprovalPlanView(rows, *a.PlanArtifact))
}

// --- schedules / rules / silences -------------------------------------------

func (s *Server) apiSchedules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListSchedules(r.Context(), false)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindScheduleList, wire.NewScheduleList(rows))
}

func (s *Server) apiNetgroups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListNetgroups(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindNetgroupList, wire.NewNetgroupList(rows))
}

// apiChecks lists checks with each target's last observed status, its stored
// netgroup classification, its probe duration, and how long ago that status
// last changed — the same projection `check list --output json` emits
// (wire.NewCheckList joins each check's check_state rows; the stored values are
// authoritative).
func (s *Server) apiChecks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListChecks(r.Context(), false)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	states, err := s.checkStates(r.Context(), rows)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindCheckList, wire.NewCheckList(rows, states))
}

func (s *Server) apiRules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListRules(r.Context(), false)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindRuleList, wire.NewRuleList(rows))
}

// apiSilences lists silences. Like `silence list`, it shows active-only by
// default; ?all=true includes expired silences.
func (s *Server) apiSilences(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListSilences(r.Context(), !queryTrue(r, "all"))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindSilenceList, wire.NewSilenceList(rows))
}

// apiMutes lists notification mutes. Like `mute list`, it shows active-only by
// default; ?all=true includes expired mutes. A mute is the notify-layer hold
// (the job still runs; only its announcement is held) — distinct from a
// silence, which suppresses the rule.
func (s *Server) apiMutes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListNotifyMutes(r.Context(), !queryTrue(r, "all"))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindMuteList, wire.NewMuteList(rows))
}

// --- events -----------------------------------------------------------------

// apiEvents lists received events with the CLI's filters (source, outcome,
// limit), defaulting to the CLI's limit of 25.
func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.EventFilter{Source: q.Get("source"), Limit: 25}
	if v := q.Get("outcome"); v != "" {
		if !knownOutcome(v) {
			s.badRequest(w, "outcome "+strconv.Quote(v)+" is not an event outcome")
			return
		}
		filter.Outcomes = []store.EventOutcome{store.EventOutcome(v)}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.badRequest(w, "limit must be a non-negative integer")
			return
		}
		filter.Limit = n
	}
	rows, err := s.store.ListEvents(r.Context(), filter)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindEventList, wire.NewEventList(rows))
}

// apiEventsSummary returns the event funnel as counts by outcome — the same
// outcome names EventView.Outcome and `event list --outcome` use — so the
// phone companion's Events screen reads one object instead of paging
// GET /events to tally the dispositions itself. It reads through
// store.CountEventsByOutcome, the same event table and outcome column
// ListEvents filters on, so this can never disagree with GET /events for the
// same rows. All-time, matching apiEvents's own unwindowed row listing.
func (s *Server) apiEventsSummary(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.CountEventsByOutcome(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindEventSummary, wire.NewEventSummaryView(counts))
}

func (s *Server) apiEventGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "event id must be a positive integer")
		return
	}
	e, err := s.store.GetEvent(r.Context(), id)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindEvent, wire.NewEventView(e))
}

// --- secrets (metadata only) / backups --------------------------------------

// apiSecrets returns secret METADATA only. It reuses the exact values-free
// SecretView: nonce, ciphertext, and AAD have no field on that type, so a sealed
// value cannot leak through this route even by accident.
func (s *Server) apiSecrets(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListSecrets(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindSecretList, wire.NewSecretList(rows))
}

func (s *Server) apiBackups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListBackupTargets(r.Context(), true)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindBackupList, wire.NewBackupTargetList(rows))
}

// --- runners (registered execution-environment recipes) ---------------------

// apiRunners lists registered runner definitions (byo-runner.md §2). A runner
// is registered for the deferred container tier; nothing here executes one.
func (s *Server) apiRunners(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListRunners(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindRunnerList, wire.NewRunnerList(rows))
}

// apiRunnerGet returns one runner by name. An unknown name is 404.
func (s *Server) apiRunnerGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.badRequest(w, "runner name must not be empty")
		return
	}
	rn, err := s.store.RunnerByName(r.Context(), name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindRunner, wire.NewRunnerView(rn))
}

// --- health -------------------------------------------------------------

// healthCacheTTL bounds how stale a served health summary may be. A doctor
// probe forks a subprocess per optional tool and opens the secrets envelope, so
// running it per request let the LOWEST scope drive unbounded process creation
// and repeated keyring unwraps. Ten seconds is shorter than any human refresh
// and shorter than the scrape intervals an agent polls at, so the summary a
// caller reads is still the current one.
const healthCacheTTL = 10 * time.Second

// healthCache holds the last probe and when it expires. The mutex is held for
// the length of a probe, so concurrent readers share one run instead of each
// forking their own.
type healthCache struct {
	mu       sync.Mutex
	view     wire.HealthView
	expires  time.Time
	computed bool
}

// healthView returns the cached doctor summary, refreshing it when it has
// expired.
func (s *Server) healthView(ctx context.Context) wire.HealthView {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	now := s.now()
	if s.health.computed && now.Before(s.health.expires) {
		return s.health.view
	}
	s.health.view = s.probe(ctx)
	s.health.expires = now.Add(healthCacheTTL)
	s.health.computed = true
	return s.health.view
}

// apiHealth returns the same subsystem summary `doctor --output json` does,
// served from a short-lived cache (healthCacheTTL). It never fails: an
// unreachable subsystem is a per-entry status, not an HTTP error, matching the
// CLI's fault-tolerant JSON path (internal/health.Probe never short-circuits on
// a data-dir fault).
func (s *Server) apiHealth(w http.ResponseWriter, r *http.Request) {
	s.apiSuccess(w, wire.KindDoctor, s.healthView(r.Context()))
}

// healthOpenStore hands internal/health the server's already-open store
// rather than a fresh connection: the web API is a long-running daemon, not a
// one-shot CLI invocation, so there is no connection to close per probe.
func (s *Server) healthOpenStore() (health.Store, func() error, error) {
	return s.store, func() error { return nil }, nil
}

// --- small validators (mirrors of the CLI's, using store constants) ---------

func queryTrue(r *http.Request, key string) bool {
	return r.URL.Query().Get(key) == "true"
}

func knownJobState(s string) bool {
	switch store.JobState(s) {
	case store.StateQueued, store.StateAwaitingApproval, store.StateRunning,
		store.StateSucceeded, store.StateDegraded, store.StateFailed,
		store.StateCancelled, store.StateTimedOut, store.StateInterrupted:
		return true
	default:
		return false
	}
}

func knownOutcome(s string) bool {
	switch store.EventOutcome(s) {
	case store.OutcomeIgnored, store.OutcomeMatched, store.OutcomeSuppressed,
		store.OutcomeGated, store.OutcomeFired, store.OutcomeError:
		return true
	default:
		return false
	}
}
