package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/checkprobe"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// This file mounts the operator HTTP/JSON WRITE API (agent-interface §3.4) on
// the SAME Decision 0009 listener as the reads. Every mutating route routes through
// the SAME internal/service verbs the CLI calls, so no client has a divergent
// execution path (AGENTS.md invariant 5): the verb assembles the queue.Spec and
// makes the gate decision, and this face supplies a PURE-ENQUEUE Enqueuer —
// queue the row and return its id at once, never inline-run or stream like the
// CLI's interactive enqueuer. A running daemon claims and runs the row.
//
// Scope is gated BEFORE any work (agent-interface §4): plan/apply need operator,
// a decision needs the approve scope — W and A are disjoint here, so a token
// handed to an agent cannot both raise a gate and release it. An
// out-of-scope token is a wire forbidden envelope before the input is even built.
//
// POST /playbooks/run resolves an inventory name to its path (and a --source
// playbook onto its checkout) through the SAME service resolvers the CLI uses,
// then enqueues through RunPlaybook. A --source whose checkout declares hooks is
// refused: that lifecycle is a composed workflow the CLI owns, not one queued
// play.
//
// A saved plan applies by artifact id: the apply enqueues against the SEALED
// plan path, and the daemon that claims the row decrypts it into a run-private
// plaintext for the one run that consumes it (workspaceDriver), so this
// pure-enqueue face never leaves a plaintext plan in the shared data dir. A plan
// started through /workspaces/{name}/plan carries the "api" trigger, so the
// daemon seals its -out into a consumable artifact and records its policy
// verdict post-run — the id an apply-by-id then consults.

// Write-response kinds. The wire package has no write projections yet, so
// these presentation-only shapes live here; they carry identifiers and states
// only, never values (invariant 4).
const (
	kindJobEnqueued     = "job.enqueued"
	kindApprovalPending = "approval.pending"
	kindDecision        = "approval.decision"
	// kindSilence names one created silence, mirroring wire.KindJob's "job.get"
	// singular next to the plural KindSilenceList the read side already emits.
	kindSilence = "silence.get"
	// kindMute names one created notification mute, the singular next to the
	// plural wire.KindMuteList the read side emits.
	kindMute = "mute.get"
	// kindJobNotify names a registered one-shot notify request.
	kindJobNotify = "job.notify"
)

// jobEnqueuedView is the body of a clean enqueue: the job id an agent follows,
// and its state at enqueue time (queued).
type jobEnqueuedView struct {
	JobID int64  `json:"job_id"`
	State string `json:"state,omitempty"`
}

// approvalPendingView is the body of a gated apply: the job sits in
// awaiting_approval and this approval releases it. ttl_seconds and expires_at
// let an agent read the fail-closed rejection at TTL as intended, not as an
// error (agent-interface §4).
type approvalPendingView struct {
	ApprovalID     int64  `json:"approval_id"`
	JobID          int64  `json:"job_id"`
	State          string `json:"state"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	TTLSeconds     int64  `json:"ttl_seconds,omitempty"`
	Policy         string `json:"policy,omitempty"`
	PlanArtifactID int64  `json:"plan_artifact_id,omitempty"`
}

// pendingView renders a gated enqueue's approval, with its TTL counted from now.
func (s *Server) pendingView(a service.GatedApproval) approvalPendingView {
	return approvalPendingView{
		ApprovalID:     a.ApprovalID,
		JobID:          a.JobID,
		State:          string(a.State),
		ExpiresAt:      a.ExpiresAt.UTC().Format(time.RFC3339),
		TTLSeconds:     int64(a.ExpiresAt.Sub(s.now()).Seconds()),
		Policy:         a.Policy,
		PlanArtifactID: a.PlanArtifactID,
	}
}

// decisionView is the outcome of a decision: which approval, the settled
// outcome, and the terminal notice (one paragraph, values-free).
type decisionView struct {
	ApprovalID int64  `json:"approval_id"`
	Outcome    string `json:"outcome,omitempty"`
	Message    string `json:"message,omitempty"`
}

// registerWriteAPI mounts the /api/v1 POST routes. Go 1.22 method-aware routing
// means a GET (or any non-POST) on one of these paths, and a POST on a GET-only
// read path, both fall through to the mux's own 405 — the contract's write/read
// method-mismatch rule, for free.
func (s *Server) registerWriteAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/approvals/{id}/decision",
		s.apiGuard(bearerOnly(canApprove), writeDeny("decide approvals", "approve"), s.apiApprovalDecision))
	mux.HandleFunc("POST /api/v1/workspaces/{name}/plan",
		s.apiGuard(bearerOnly(canWrite), writeDeny("enqueue writes", "operator"), s.apiWorkspacePlan))
	mux.HandleFunc("POST /api/v1/workspaces/{name}/apply",
		s.apiGuard(bearerOnly(canWrite), writeDeny("enqueue writes", "operator"), s.apiWorkspaceApply))
	mux.HandleFunc("POST /api/v1/workspaces/{name}/destroy",
		s.apiGuard(bearerOnly(canWrite), writeDeny("enqueue writes", "operator"), s.apiWorkspaceDestroy))
	mux.HandleFunc("POST /api/v1/playbooks/run",
		s.apiGuard(bearerOnly(canWrite), writeDeny("enqueue writes", "operator"), s.apiPlaybookRun))
	mux.HandleFunc("POST /api/v1/adhoc/run",
		s.apiGuard(bearerOnly(canWrite), writeDeny("enqueue writes", "operator"), s.apiAdhocRun))
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel",
		s.apiGuard(bearerOnly(canWrite), writeDeny("cancel jobs", "operator"), s.apiJobCancel))
	mux.HandleFunc("POST /api/v1/jobs/{id}/notify",
		s.apiGuard(bearerOnly(canWrite), writeDeny("request job notifications", "operator"), s.apiJobNotify))
	mux.HandleFunc("POST /api/v1/silences",
		s.apiGuard(bearerOnly(canWrite), writeDeny("create silences", "operator"), s.apiSilenceCreate))
	mux.HandleFunc("DELETE /api/v1/silences/{id}",
		s.apiGuard(bearerOnly(canWrite), writeDeny("lift silences", "operator"), s.apiSilenceDelete))
	mux.HandleFunc("POST /api/v1/mutes",
		s.apiGuard(bearerOnly(canWrite), writeDeny("create mutes", "operator"), s.apiMuteCreate))
	mux.HandleFunc("DELETE /api/v1/mutes/{id}",
		s.apiGuard(bearerOnly(canWrite), writeDeny("lift mutes", "operator"), s.apiMuteDelete))
	mux.HandleFunc("POST /api/v1/checks",
		s.apiGuard(bearerOnly(canWrite), writeDeny("manage checks", "operator"), s.apiCheckCreate))
	mux.HandleFunc("PUT /api/v1/checks/{name}",
		s.apiGuard(bearerOnly(canWrite), writeDeny("manage checks", "operator"), s.apiCheckUpdate))
	mux.HandleFunc("POST /api/v1/checks/{name}/enable",
		s.apiGuard(bearerOnly(canWrite), writeDeny("manage checks", "operator"), s.apiCheckEnable))
	mux.HandleFunc("POST /api/v1/checks/{name}/disable",
		s.apiGuard(bearerOnly(canWrite), writeDeny("manage checks", "operator"), s.apiCheckDisable))
	mux.HandleFunc("DELETE /api/v1/checks/{name}",
		s.apiGuard(bearerOnly(canWrite), writeDeny("manage checks", "operator"), s.apiCheckDelete))
	// probe runs the network but writes nothing (D3), so it takes the read
	// floor: any valid token may ask what a check sees right now.
	mux.HandleFunc("POST /api/v1/checks/{name}/probe",
		s.apiGuard(bearerOnly(canRead), writeDeny("probe checks", "readonly"), s.apiCheckProbe))
	mux.HandleFunc("POST /api/v1/netgroups",
		s.apiGuard(bearerOnly(canWrite), writeDeny("manage netgroups", "operator"), s.apiNetgroupCreate))
	mux.HandleFunc("PUT /api/v1/netgroups/{name}",
		s.apiGuard(bearerOnly(canWrite), writeDeny("manage netgroups", "operator"), s.apiNetgroupUpdate))
	mux.HandleFunc("DELETE /api/v1/netgroups/{name}",
		s.apiGuard(bearerOnly(canWrite), writeDeny("manage netgroups", "operator"), s.apiNetgroupDelete))
	mux.HandleFunc("POST /api/v1/hosts/self/requests",
		s.apiGuard(bearerOnly(canHostRequest), writeDeny("request jobs", "host"), s.apiHostRequest))
}

// bearerOnly narrows a scope predicate to callers presenting an explicit bearer
// credential. A browser session is AMBIENT: the cookie rides any request the
// browser is talked into making, and SameSite=Strict does not scope to a port,
// so a second app on the same host, an XSS anywhere on it, or a local page under
// --insecure-http could drive operator-scope writes with no CSRF token, no
// Origin check and no preflight. A machine face has no reason to
// accept an ambient credential; the browser's own decide route (handlers.go) is
// where a session acts, and it is same-origin form-driven.
func bearerOnly(allow func(identity) bool) func(identity) bool {
	return func(id identity) bool { return id.via == "bearer" && allow(id) }
}

// writeDeny is the refusal a write route renders, naming both conditions the
// caller has to meet, since one predicate carries both.
func writeDeny(verb, scope string) string {
	return "this request may not " + verb + ": it needs an Authorization: Bearer token " +
		"of the " + scope + " scope (a browser session may not drive machine writes)"
}

// enqueuer returns a PURE-ENQUEUE Enqueuer: it inserts the queued (or gated) row
// through the one queue and returns immediately. Its driver refuses to run —
// this process only enqueues; the daemon that owns the real queue claims and
// runs the row. It is the API analogue of the CLI's enqueueOnly path.
func (s *Server) enqueuer() apiEnqueuer {
	return apiEnqueuer{q: queue.New(s.store, apiNoRunDriver{}, queue.Options{Workers: 1})}
}

type apiEnqueuer struct{ q *queue.Queue }

func (e apiEnqueuer) Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error) {
	return e.q.Enqueue(ctx, spec)
}

func (e apiEnqueuer) EnqueueGated(ctx context.Context, spec queue.Spec) (store.Job, error) {
	return e.q.EnqueueGated(ctx, spec)
}

// apiNoRunDriver is the driver of the API's enqueue-only queue. It is never
// drained; refusing loudly keeps a future caller from draining it and racing the
// daemon for the row.
type apiNoRunDriver struct{}

func (apiNoRunDriver) Run(context.Context, executor.Spec, executor.EventSink) (executor.Result, error) {
	return executor.Result{}, errors.New("web: the API only enqueues; the daemon runs the job")
}

// service builds the operational core over the server's store, data dir, and
// clock — the same verbs the CLI drives, differing only in the Enqueuer.
func (s *Server) service() *service.Service {
	return service.New(s.store, s.dataDir, s.clock)
}

// writeRequest is the optional JSON body a mutating request may carry. All
// fields are optional; an empty body is the common case (a bare plan/apply).
type writeRequest struct {
	RunSecrets string `json:"run_secrets"`
	Timeout    string `json:"timeout"`
	// Plan names a saved plan artifact by id for apply --plan; empty is a fresh apply.
	Plan string `json:"plan"`
	// Runner names a registered runner whose memory_max/cpu_quota the sandboxed
	// execution path should enforce as a per-job cgroup cap, mirroring the
	// local --runner flag. Empty is the default, uncapped path.
	Runner string `json:"runner"`
}

// decodeWrite reads the optional JSON body and the parsed timeout. It renders
// the 400 itself and reports ok=false when the body or the timeout is malformed.
func (s *Server) decodeWrite(w http.ResponseWriter, r *http.Request) (writeRequest, time.Duration, bool) {
	var req writeRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.badRequest(w, "request body is not valid JSON")
			return req, 0, false
		}
	}
	var timeout time.Duration
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil {
			s.badRequest(w, "timeout is not a valid Go duration")
			return req, 0, false
		}
		timeout = d
	}
	return req, timeout, true
}

// actorFromRequest resolves the caller identity into the service.Actor a
// write records on its job's event ledger — the durable record the approval
// decision path already gets by threading identity into reactive.Decide
// (apiApprovalDecision below). Only the token's NAME travels, never its
// secret (AGENTS.md invariant 4).
func actorFromRequest(r *http.Request) service.Actor {
	ident, ok := identityOf(r.Context())
	if !ok {
		return service.Actor{}
	}
	return service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name}
}

// Route labels for the idempotency namespace. They are stable strings, not
// r.URL.Path: a path carries the workspace name, and two writes to the same
// workspace under one key SHOULD dedup.
const (
	routePlan     = "workspace.plan"
	routeApply    = "workspace.apply"
	routeDestroy  = "workspace.destroy"
	routePlaybook = "playbook.run"
	routeAdhoc    = "adhoc.run"
)

// idempotencyKey reads the caller-settable Idempotency-Key header and namespaces
// it by the authenticated principal and the route. Threaded onto the service
// Input, it dedups a retried request to one job (queue.go:117-126), so a
// retrying agent never double-enqueues.
//
// The namespace is the security half: the raw header is one global key space, so
// a guessable key ("deploy-<date>", a ticket id) planted by one token silently
// swallowed another token's write — the victim got a 200 naming a job it did not
// create, its own write never enqueued. Scoping by token id keeps
// a caller's keys to itself; scoping by route keeps its own plan from swallowing
// its own apply; and both prefixes make the queue's reserved `wf:` workflow step
// keys unaddressable from this face.
func idempotencyKey(r *http.Request, route string) string {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return ""
	}
	var tokenID int64
	if ident, ok := identityOf(r.Context()); ok {
		tokenID = ident.token.ID
	}
	return "api:" + strconv.FormatInt(tokenID, 10) + ":" + route + ":" + key
}

// tofuArtifactsDir is where a plan's -out file lands, mirroring the CLI's
// tofuDirs. It sits inside the 0700 data dir: plan files hold resource values in
// the clear until they are sealed.
func (s *Server) tofuArtifactsDir() (string, error) {
	dir := filepath.Join(s.dataDir, "artifacts", "tofu")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// --- workspace plan / apply -------------------------------------------------

// apiWorkspacePlan enqueues a tofu plan for a workspace through PlanWorkspace and
// returns the job id. The plan runs under a running daemon; sealing its output
// into a consumable artifact is a CLI-resident post-run step (see this file's
// header) and is not done here.
func (s *Server) apiWorkspacePlan(w http.ResponseWriter, r *http.Request) {
	req, timeout, ok := s.decodeWrite(w, r)
	if !ok {
		return
	}
	ws, err := s.store.WorkspaceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	artifacts, err := s.tofuArtifactsDir()
	if err != nil {
		s.log.Error("api: artifacts dir", "err", err)
		s.apiError(w, http.StatusInternalServerError, wire.NewError(wire.CodeInternal, "cannot prepare artifacts dir"))
		return
	}
	res, err := s.service().PlanWorkspace(r.Context(), s.enqueuer(), service.PlanInput{
		Workspace:      ws.Name,
		TofuDir:        ws.TofuDir,
		ArtifactsDir:   artifacts,
		RunSecrets:     req.RunSecrets,
		RunnerName:     req.Runner,
		IdempotencyKey: idempotencyKey(r, routePlan),
		Timeout:        timeout,
		// The "api" trigger tells the daemon to seal this plan's -out file into a
		// consumable artifact post-run; a CLI plan seals its own.
		Trigger: service.TriggerAPI,
		Actor:   actorFromRequest(r),
	})
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, kindJobEnqueued, jobEnqueuedView{JobID: res.Job.ID, State: string(res.Job.State)})
}

// apiWorkspaceApply enqueues a tofu apply through ApplyWorkspace. A fresh apply
// (no plan) auto-approves and enqueues plainly. With a saved plan named by
// artifact id, the plan's recorded policy verdict routes it: a deny is refused
// (409), a require_approval lands the job in awaiting_approval behind a fresh
// approval (the pending envelope), and a plain saved-plan apply is refused for
// now because its decrypt step is CLI-resident.
func (s *Server) apiWorkspaceApply(w http.ResponseWriter, r *http.Request) {
	req, timeout, ok := s.decodeWrite(w, r)
	if !ok {
		return
	}
	ws, err := s.store.WorkspaceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.apiFail(w, err)
		return
	}

	svc := s.service()
	var saved *store.Artifact
	var gate service.PlanGate
	if req.Plan != "" {
		id, perr := strconv.ParseInt(req.Plan, 10, 64)
		if perr != nil || id <= 0 {
			s.badRequest(w, "plan must be a saved plan artifact id")
			return
		}
		art, gerr := s.store.GetArtifact(r.Context(), id)
		if gerr != nil {
			s.apiFail(w, gerr)
			return
		}
		if art.Kind != reactive.PlanArtifactKind {
			s.badRequest(w, "artifact "+req.Plan+" is not a plan")
			return
		}
		saved = &art
		// Fail-closed adjudication: a plan on a policied workspace with no
		// recorded verdict is unconsumable, not unpoliced.
		gate, err = svc.PlanGateForPlan(r.Context(), ws.Name, art)
		var mismatch *service.PlanWorkspaceMismatchError
		switch {
		case errors.As(err, &mismatch):
			// A plan named for another workspace is a bad argument, refused
			// before anything is enqueued or decrypted.
			s.badRequest(w, err.Error())
			return
		case err != nil:
			s.apiFail(w, err)
			return
		}
	}

	res, err := svc.ApplyWorkspace(r.Context(), s.enqueuer(), s.deriver, service.ApplyInput{
		Workspace:       ws.Name,
		WorkspaceSerial: ws.Serial,
		TofuDir:         ws.TofuDir,
		RunSecrets:      req.RunSecrets,
		RunnerName:      req.Runner,
		IdempotencyKey:  idempotencyKey(r, routeApply),
		Timeout:         timeout,
		SavedPlan:       saved,
		Gate:            gate,
		Actor:           actorFromRequest(r),
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		if saved == nil {
			return executor.TofuArgv("apply", "-auto-approve"), nil, func() {}, nil
		}
		// Enqueue the apply against the SEALED plan path; the daemon that claims
		// the row decrypts it into a run-private plaintext for the one run that
		// consumes it (workspaceDriver), so no plaintext plan is ever left in the
		// shared data dir by this pure-enqueue face.
		return executor.TofuArgv("apply", saved.Path), saved, func() {}, nil
	})

	var denied *service.PlanDeniedError
	switch {
	case errors.As(err, &denied):
		s.apiError(w, http.StatusConflict, wire.NewError(wire.CodeConflict, err.Error()))
		return
	case err != nil:
		s.apiFail(w, err)
		return
	}

	if res.Gated {
		s.apiSuccess(w, kindApprovalPending, s.pendingView(res.Approval))
		return
	}

	run := res.Run
	defer run.Release()
	s.apiSuccess(w, kindJobEnqueued, jobEnqueuedView{JobID: run.Job.ID, State: string(run.Job.State)})
}

// apiWorkspaceDestroy enqueues a tofu destroy through DestroyWorkspace — the
// one-queue twin of the apply, minus the plan: a destroy has nothing for a plan
// policy to read, so it is a plain enqueue, never gated. The argv the row
// records is `tofu destroy -auto-approve`; the daemon that claims it runs the
// same workspace driver as any other tofu job.
func (s *Server) apiWorkspaceDestroy(w http.ResponseWriter, r *http.Request) {
	req, timeout, ok := s.decodeWrite(w, r)
	if !ok {
		return
	}
	ws, err := s.store.WorkspaceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	res, err := s.service().DestroyWorkspace(r.Context(), s.enqueuer(), service.DestroyInput{
		Workspace:      ws.Name,
		TofuDir:        ws.TofuDir,
		RunSecrets:     req.RunSecrets,
		RunnerName:     req.Runner,
		IdempotencyKey: idempotencyKey(r, routeDestroy),
		Timeout:        timeout,
		Trigger:        service.TriggerAPI,
		Actor:          actorFromRequest(r),
	})
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, kindJobEnqueued, jobEnqueuedView{JobID: res.Job.ID, State: string(res.Job.State)})
}

// --- approval decision ------------------------------------------------------

// apiFault is an error that names both its taxonomy code and its HTTP status.
// The decision path's refusals (a scope-disjointness refusal, a surface with no
// decider, a stale plan) have no store sentinel for wire.ErrorFor to map from,
// so they carry the mapping themselves — which is what lets ONE decision
// implementation serve two machine faces that render boundaries differently.
type apiFault struct {
	status int
	err    wire.Error
}

func (f *apiFault) Error() string { return f.err.Message }

// WireCode lets a non-HTTP face (the MCP tool seam) recover the taxonomy code
// without knowing anything about HTTP status.
func (f *apiFault) WireCode() string { return f.err.Code }

func fault(status int, code, msg string) *apiFault {
	return &apiFault{status: status, err: wire.NewError(code, msg)}
}

// decideApproval settles one approval through the reactive engine — the single
// fail-closed place a human's answer enters the system, whatever surface asked
// (the same s.decider.Decide the browser's handleDecide calls). The token is
// derived server-side, never carried by the caller: a scope is not a capability
// link, and returning one would silently promote its holder to approver.
//
// It is shared by every machine face — the /api/v1 decision route and the MCP
// approve tools — so the separation-of-duty refusal below cannot hold on one
// face and be forgotten on the other. Scope is NOT checked here:
// the caller's guard has already established that A was satisfied, and this is
// the second, principal-level half of the same rule.
func (s *Server) decideApproval(ctx context.Context, id int64, decision string, ident identity) (decisionView, error) {
	if decision != reactive.DecisionApprove && decision != reactive.DecisionReject {
		return decisionView{}, fault(http.StatusBadRequest, codeInvalidArgument,
			"decision must be approve or reject")
	}
	if s.decider == nil {
		return decisionView{}, fault(http.StatusServiceUnavailable, wire.CodeInternal,
			"approvals cannot be decided from this surface")
	}

	approval, err := s.store.GetApproval(ctx, id)
	if err != nil {
		return decisionView{}, err
	}

	self, err := s.decidedBySelf(ctx, approval.JobID, ident)
	if err != nil {
		s.log.Error("api: read job actor", "approval", id, "err", err)
		return decisionView{}, fault(http.StatusInternalServerError, wire.CodeInternal,
			"cannot read the job's actor")
	}
	if self {
		return decisionView{}, fault(http.StatusForbidden, wire.CodeForbidden,
			"this token asked for the gated job; it may not decide its own approval")
	}

	token, _, err := reactive.ApprovalToken(s.deriver, approval.JobID)
	if err != nil {
		s.log.Error("api: derive approval token", "approval", id, "err", err)
		return decisionView{}, fault(http.StatusInternalServerError, wire.CodeInternal,
			"cannot derive approval token")
	}

	result, derr := s.decider.Decide(ctx, id, decision, token, "api:"+ident.token.Name)
	if errors.Is(derr, reactive.ErrStalePlan) {
		// A safety rejection, not a server fault: the workspace moved under the
		// saved plan, so the job was not released. Carry the structured movement
		// so a client reads why from fields, not the message; a best-effort
		// lookup, since the refusal stands whether or not the block resolves.
		e := wire.ErrorFor(derr)
		if stale, serr := s.approvalStaleView(ctx, approval); serr == nil {
			e.Stale = stale
		}
		return decisionView{}, &apiFault{status: http.StatusConflict, err: e}
	}
	if derr != nil {
		s.log.Error("api: decide", "approval", id, "err", derr)
		return decisionView{}, derr
	}
	return decisionView{ApprovalID: id, Outcome: result.Outcome, Message: result.Message}, nil
}

// apiApprovalDecision is the HTTP rendering of decideApproval: it parses the
// path and body, then hands the whole decision to the shared path.
func (s *Server) apiApprovalDecision(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "approval id must be a positive integer")
		return
	}

	var req struct {
		Decision string `json:"decision"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err := json.Unmarshal(body, &req); err != nil {
		s.badRequest(w, "request body must be {\"decision\":\"approve\"|\"reject\"}")
		return
	}

	ident, _ := identityOf(r.Context())
	view, err := s.decideApproval(r.Context(), id, req.Decision, ident)
	if err != nil {
		var f *apiFault
		if errors.As(err, &f) {
			s.apiError(w, f.status, f.err)
			return
		}
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, kindDecision, view)
}

// --- playbook run -----------------------------------------------------------

// playbookRunRequest is the JSON body of a POST /playbooks/run. The playbook
// path and any --source, --inventory names are resolved server-side through the
// SAME service resolvers the CLI uses, so an operator's names turn into the same
// paths whichever face asked. extra_vars is key=val or @file, validated by the
// shared passthrough seam; secret values never travel here (the vault password
// and run-secrets keep their own out-of-argv paths).
type playbookRunRequest struct {
	Playbook            string   `json:"playbook"`
	Inventory           string   `json:"inventory"`
	VaultProject        string   `json:"vault_project"`
	RunSecrets          string   `json:"run_secrets"`
	Source              string   `json:"source"`
	Limit               string   `json:"limit"`
	Tags                string   `json:"tags"`
	SkipTags            string   `json:"skip_tags"`
	Check               bool     `json:"check"`
	Diff                bool     `json:"diff"`
	ExtraVars           []string `json:"extra_vars"`
	TolerateUnreachable bool     `json:"tolerate_unreachable"`
	Timeout             string   `json:"timeout"`
	// Runner names a registered runner whose memory_max/cpu_quota the sandboxed
	// execution path should enforce as a per-job cgroup cap, mirroring the
	// local --runner flag. Empty is the default, uncapped path.
	Runner string `json:"runner"`
	// EgressPorts are the outbound TCP ports the sandboxed run may open on top
	// of the ansible kind's SSH port 22, validated (1..65535) before enqueue.
	EgressPorts []int `json:"egress_ports"`
	// SSHIdentityFiles are ssh identity key files on the daemon the run may
	// read. Each must be one the operator allowed with serve
	// --allow-ssh-identity, because the caller names paths on a host it does
	// not control.
	SSHIdentityFiles []string `json:"ssh_identity_files"`
}

// apiPlaybookRun enqueues an ansible playbook through RunPlaybook and returns the
// job id. It resolves the inventory name and the --source playbook path with the
// shared service verbs, then hands the parsed input to the pure-enqueue Enqueuer;
// a running daemon claims and runs the ansible job. A --source whose checkout
// declares hooks is refused: that lifecycle is a composed workflow the CLI owns,
// not a single queued play, so it is not offered on this face yet.
func (s *Server) apiPlaybookRun(w http.ResponseWriter, r *http.Request) {
	var req playbookRunRequest
	if !s.decodeJSON(w, r, &req, maxRunBody) {
		return
	}
	if req.Playbook == "" {
		s.badRequest(w, "playbook is required")
		return
	}
	var timeout time.Duration
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil {
			s.badRequest(w, "timeout is not a valid Go duration")
			return
		}
		timeout = d
	}

	passthrough, vars, err := service.AnsiblePassthrough(req.Limit, req.Tags, req.SkipTags, req.Check, req.Diff, req.ExtraVars)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}

	// A junk egress port is refused here, at the write face, so a typo never
	// reaches a job row.
	egressPorts, err := store.NormalizeEgressPorts(req.EgressPorts)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}

	identities, err := s.allowedIdentities(req.SSHIdentityFiles)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}

	svc := s.service()
	playbook := req.Playbook
	if req.Source != "" {
		// Validate the NAME before it is spliced into any path: the override
		// path below is built from it, ahead of the store lookup that would
		// otherwise be what constrains it.
		if err := service.ValidSourceName(req.Source); err != nil {
			s.badRequest(w, err.Error())
			return
		}
		// The override file convention mirrors the CLI's hooksOverridePath.
		override := filepath.Join(s.dataDir, "hooks", req.Source+".yml")
		hooked, abs, serr := svc.ResolveSourcePlaybook(r.Context(), override, req.Source, req.Playbook)
		if errors.Is(serr, service.ErrSourcePathEscape) {
			// A playbook outside the source's checkout is a bad argument: the run
			// would execute a tree the source's signature never covered.
			s.badRequest(w, serr.Error())
			return
		}
		if serr != nil {
			s.apiFail(w, serr)
			return
		}
		if hooked {
			s.apiError(w, http.StatusNotImplemented, wire.NewError(wire.CodeInternal,
				"source "+req.Source+" declares hooks; its lifecycle workflow is not offered on this face yet"))
			return
		}
		playbook = abs
	}
	playbook, err = filepath.Abs(playbook)
	if err != nil {
		s.badRequest(w, "playbook path cannot be resolved")
		return
	}

	invPath, invName, err := s.resolveRunInventory(r.Context(), req.Inventory)
	if errors.Is(err, errRelativeInventory) {
		s.badRequest(w, err.Error())
		return
	}
	if err != nil {
		s.apiFail(w, err)
		return
	}

	// A --source run executes from its checkout so the checkout's ./ansible.cfg
	// (and the relative inventory/roles it declares) is honored, mirroring the
	// CLI and the browser launcher; without it the run falls back to the job
	// scratch dir and the config is never discovered.
	var dir string
	var dirReadOnly bool
	if req.Source != "" {
		dir, dirReadOnly, err = s.resolveSourceCheckout(r.Context(), req.Source)
		if err != nil {
			s.apiFail(w, err)
			return
		}
	}

	res, err := svc.RunPlaybook(r.Context(), s.enqueuer(), service.PlaybookInput{
		PlaybookPath:        playbook,
		InventoryPath:       invPath,
		InventoryName:       invName,
		VaultProject:        req.VaultProject,
		RunSecrets:          req.RunSecrets,
		Source:              req.Source,
		Dir:                 dir,
		DirReadOnly:         dirReadOnly,
		Passthrough:         passthrough,
		ExtraVars:           vars,
		TolerateUnreachable: req.TolerateUnreachable,
		IdempotencyKey:      idempotencyKey(r, routePlaybook),
		Timeout:             timeout,
		RunnerName:          req.Runner,
		EgressPorts:         egressPorts,
		SSHIdentityFiles:    identities,
		Actor:               actorFromRequest(r),
	})
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, kindJobEnqueued, jobEnqueuedView{JobID: res.Job.ID, State: string(res.Job.State)})
}

// adhocRunRequest is the body of an ad-hoc run: the `exocomp command` and
// `exocomp ansible` inputs as fields. Source and Inventory are resolved here, on
// the daemon, exactly as for a playbook run.
type adhocRunRequest struct {
	// Runbook is "command" or "ansible", the label the job list tells the two
	// verbs apart by. Empty is "command".
	Runbook             string   `json:"runbook"`
	Host                string   `json:"host"`
	Module              string   `json:"module"`
	Args                string   `json:"args"`
	Become              bool     `json:"become"`
	Source              string   `json:"source"`
	Inventory           string   `json:"inventory"`
	VaultProject        string   `json:"vault_project"`
	RunSecrets          string   `json:"run_secrets"`
	Runner              string   `json:"runner"`
	RequireApproval     bool     `json:"require_approval"`
	TolerateUnreachable bool     `json:"tolerate_unreachable"`
	Timeout             string   `json:"timeout"`
	SSHIdentityFiles    []string `json:"ssh_identity_files"`
}

// apiAdhocRun enqueues one ad-hoc ansible module run through service.RunAdhoc,
// the verb the browser's run-a-command form calls. A --source run executes from
// that source's checkout, so its ansible.cfg and relative inventory apply, and
// queue admission adds the source's default egress. The answer is job.enqueued,
// or approval.pending when require_approval gated it. A privileged run is not
// offered here, as on /playbooks/run.
func (s *Server) apiAdhocRun(w http.ResponseWriter, r *http.Request) {
	var req adhocRunRequest
	if !s.decodeJSON(w, r, &req, maxRunBody) {
		return
	}
	switch req.Runbook {
	case "":
		req.Runbook = "command"
	case "command", "ansible":
	default:
		s.badRequest(w, "runbook must be command or ansible")
		return
	}
	var timeout time.Duration
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil {
			s.badRequest(w, "timeout is not a valid Go duration")
			return
		}
		timeout = d
	}
	// RunAdhoc guards these too, but as plain errors; checked here they are the
	// caller's bad input (400), not a server fault.
	module := req.Module
	if module == "" {
		module = "command"
	}
	for _, err := range []error{service.ValidHostPattern(req.Host), service.ValidModuleName(module),
		service.ValidModuleArgs(req.Args)} {
		if err != nil {
			s.badRequest(w, err.Error())
			return
		}
	}
	identities, err := s.allowedIdentities(req.SSHIdentityFiles)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}

	var dir string
	var dirReadOnly bool
	if req.Source != "" {
		if err := service.ValidSourceName(req.Source); err != nil {
			s.badRequest(w, err.Error())
			return
		}
		dir, dirReadOnly, err = s.resolveSourceCheckout(r.Context(), req.Source)
		if errors.Is(err, service.ErrSourcePathEscape) {
			s.badRequest(w, err.Error())
			return
		}
		if err != nil {
			s.apiFail(w, err)
			return
		}
	}

	svc := s.service()
	invPath, invName, err := s.resolveRunInventory(r.Context(), req.Inventory)
	if errors.Is(err, errRelativeInventory) {
		s.badRequest(w, err.Error())
		return
	}
	if err != nil {
		s.apiFail(w, err)
		return
	}

	res, err := svc.RunAdhoc(r.Context(), s.enqueuer(), s.deriver, service.AdhocInput{
		Runbook:             req.Runbook,
		Pattern:             req.Host,
		Module:              req.Module,
		Args:                req.Args,
		Become:              req.Become,
		Dir:                 dir,
		DirReadOnly:         dirReadOnly,
		InventoryPath:       invPath,
		InventoryName:       invName,
		VaultProject:        req.VaultProject,
		RunSecrets:          req.RunSecrets,
		RunnerName:          req.Runner,
		Source:              req.Source,
		RequireApproval:     req.RequireApproval,
		TolerateUnreachable: req.TolerateUnreachable,
		IdempotencyKey:      idempotencyKey(r, routeAdhoc),
		Timeout:             timeout,
		SSHIdentityFiles:    identities,
		Actor:               actorFromRequest(r),
	})
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if res.Gated {
		s.apiSuccess(w, kindApprovalPending, s.pendingView(res.Approval))
		return
	}
	s.apiSuccess(w, kindJobEnqueued, jobEnqueuedView{JobID: res.Run.Job.ID, State: string(res.Run.Job.State)})
}

// maxRunBody bounds a run request's JSON body.
const maxRunBody = 1 << 16

// decodeJSON reads an optional JSON body of at most limit bytes into v. It
// reports false, having written the answer, when the body is too large (413),
// unreadable, not one JSON value, or carries a field the route does not take
// (400): a misspelt field such as run_secret must not be dropped quietly, or
// the run goes ahead without what the caller asked for.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		s.badRequest(w, "request body could not be read")
		return false
	}
	if int64(len(body)) > limit {
		s.apiError(w, http.StatusRequestEntityTooLarge,
			wire.NewError(codeInvalidArgument, "request body exceeds "+strconv.FormatInt(limit, 10)+" bytes"))
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.badRequest(w, "request body is not valid JSON for this route: "+err.Error())
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		s.badRequest(w, "request body holds more than one JSON value")
		return false
	}
	return true
}

// errRelativeInventory refuses a run inventory that is neither registered nor
// absolute.
var errRelativeInventory = errors.New("is neither a registered inventory nor an absolute path on the daemon")

// resolveRunInventory resolves a run's inventory on the daemon: a registered
// inventory name or an absolute path on the daemon's host. A relative path
// names nothing on a face whose caller is elsewhere, so it is refused rather
// than resolved against the daemon's working directory.
func (s *Server) resolveRunInventory(ctx context.Context, value string) (path, name string, err error) {
	path, name, err = s.service().ResolveInventory(ctx, value)
	if err != nil {
		return "", "", err
	}
	if value != "" && name == "" && !filepath.IsAbs(value) {
		return "", "", fmt.Errorf("inventory %q %w", value, errRelativeInventory)
	}
	return path, name, nil
}

// allowedIdentities validates the ssh identity files a request asks its run to
// read: absolute, each one the operator allowed, and each still a regular file
// outside the data dir and the host key's secrets, so a listed path swapped
// for a symlink after serve started is refused. The refusal names the serve
// flag, since only the daemon's operator can widen the set.
func (s *Server) allowedIdentities(paths []string) ([]string, error) {
	identities, err := store.NormalizeSSHIdentityFiles(paths)
	if err != nil || len(identities) == 0 {
		return identities, err
	}
	for _, id := range identities {
		if !slices.Contains(s.sshIdentityAllow, id) {
			return nil, fmt.Errorf("ssh identity %s is not allowed on this daemon "+
				"(its operator allows files with serve --allow-ssh-identity)", id)
		}
	}
	return executor.NormalizeIdentityAllowlist(s.dataDir, identities)
}

// --- job cancel -------------------------------------------------------------

// apiJobCancel cancels a non-terminal job by moving it to cancelled through the
// SAME store transition the reactive gate uses to cancel a rejected approval's
// job (internal/queue/gate.go). It enqueues nothing and runs nothing — this
// pure-enqueue face never claims the row; a daemon that owns a running row sees
// the cancelled state and its own finalize transition (running -> terminal)
// harmlessly conflicts. A terminal job is a 409 (never a silent success), an
// unknown id a 404, and a state that raced under the read is a 409 through the
// store's own from-state guard.
func (s *Server) apiJobCancel(w http.ResponseWriter, r *http.Request) {
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
	if job.State.Terminal() {
		s.apiError(w, http.StatusConflict, wire.NewError(wire.CodeConflict,
			"job "+itoa64(id)+" is already "+string(job.State)+"; nothing to cancel"))
		return
	}
	if err := s.store.TransitionJob(r.Context(), id, job.State, store.StateCancelled); err != nil {
		s.apiFail(w, err)
		return
	}
	cancelled, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindJob, wire.NewJobView(cancelled))
}

// --- job notify (one-shot) --------------------------------------------------

// jobNotifyView is the body of a registered one-shot notify request: the job it
// watches and the state it stood at when the request was recorded.
type jobNotifyView struct {
	JobID      int64  `json:"job_id"`
	State      string `json:"state"`
	Registered bool   `json:"registered"`
}

// apiJobNotify registers an attributed, durable request to page the operator
// once when a job reaches its terminal state — the server-side half of the
// phone's "Notify me", so the daemon pages even if the app was killed. It
// enqueues and runs nothing: it writes one row to the event ledger (the durable,
// audited record, attributed to the calling token), and the reactive daemon's
// SweepJobNotifyRequests raises the single page on terminal, sharing the failure
// sweep's checkpoint so a job that also fails is never paged twice.
//
// A job that is ALREADY terminal is a 409: there is nothing to wait for, and the
// caller should read the job's terminal state directly rather than arm a page
// that would fire on the next sweep for an outcome already settled. An unknown
// id is a 404. A second request for the same job is idempotent — the same
// registration, not a second page.
func (s *Server) apiJobNotify(w http.ResponseWriter, r *http.Request) {
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
	if job.State.Terminal() {
		s.apiError(w, http.StatusConflict, wire.NewError(wire.CodeConflict,
			"job "+itoa64(id)+" is already "+string(job.State)+
				"; read its terminal state, there is nothing to wait for"))
		return
	}

	key := reactive.NotifyRequestDedupKey(id)
	seen, err := s.store.CountEventsByDedupKey(r.Context(), key)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if seen == 0 {
		actor := actorFromRequest(r)
		if _, err := s.store.CreateEvent(r.Context(), store.Event{
			Source:     reactive.SourceNotifyRequest,
			ReceivedAt: s.now(),
			DedupKey:   key,
			Outcome:    store.OutcomeMatched,
			Reason:     "one-shot notify request by " + actor.Name + " for job " + itoa64(id),
			JobID:      &id,
		}); err != nil {
			s.apiFail(w, err)
			return
		}
	}
	s.apiSuccess(w, kindJobNotify, jobNotifyView{JobID: id, State: string(job.State), Registered: true})
}

// --- silences (create / lift) -----------------------------------------------

// silenceCreateRequest is the JSON body of a POST /silences. It mirrors the
// CLI's `silence add`: a scope (alert/host/global) and a mandatory positive
// duration — a silence has no never-expires form, so `for` is required.
type silenceCreateRequest struct {
	ScopeKind  string `json:"scope_kind"`
	ScopeValue string `json:"scope_value"`
	Reason     string `json:"reason"`
	For        string `json:"for"`
}

// apiSilenceCreate mutes matching events until a deadline, through the SAME
// store.CreateSilence the CLI and the web kill switch already write. The scope
// and its value are validated here so a bad scope is a 400, not the 500 the
// store's untyped validation error would otherwise map to.
func (s *Server) apiSilenceCreate(w http.ResponseWriter, r *http.Request) {
	var req silenceCreateRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.badRequest(w, "request body is not valid JSON")
			return
		}
	}
	switch req.ScopeKind {
	case store.ScopeAlert, store.ScopeHost, store.ScopeGlobal:
	default:
		s.badRequest(w, "scope_kind must be alert, host, or global")
		return
	}
	if req.ScopeKind != store.ScopeGlobal && req.ScopeValue == "" {
		s.badRequest(w, "scope_kind "+req.ScopeKind+" needs a scope_value")
		return
	}
	if req.For == "" {
		s.badRequest(w, "for is required: a silence never expires-never (e.g. 2h)")
		return
	}
	dur, err := time.ParseDuration(req.For)
	if err != nil || dur <= 0 {
		s.badRequest(w, "for must be a positive Go duration (e.g. 2h)")
		return
	}

	now := s.now()
	sil, err := s.store.CreateSilence(r.Context(), store.Silence{
		ScopeKind:  req.ScopeKind,
		ScopeValue: req.ScopeValue,
		Reason:     req.Reason,
		CreatedAt:  now,
		ExpiresAt:  now.Add(dur),
		CreatedVia: "api",
	})
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, kindSilence, wire.NewSilenceList([]store.Silence{sil})[0])
}

// apiSilenceDelete lifts a silence before it expires, through the SAME
// store.DeleteSilence the CLI's `silence rm` calls. An unknown id is a 404
// (store.ErrNotFound), never a silent success.
func (s *Server) apiSilenceDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "silence id must be a positive integer")
		return
	}
	if err := s.store.DeleteSilence(r.Context(), id); err != nil {
		s.apiFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- mutes (create / lift) --------------------------------------------------

// muteCreateRequest is the JSON body of a POST /mutes. It mirrors the CLI's
// `mute add`: the scope ladder the phone's mute sheet offers — this-alert
// (alert), one-host (host), global — and a mandatory expiry. The expiry may be
// given as a positive duration in `for` (e.g. "2h") or as an absolute
// `expires_at` (RFC3339); one is required, since the store refuses a mute with
// no future expiry (a mute the operator forgets is how a phone goes quiet the
// week the backups fail).
type muteCreateRequest struct {
	ScopeKind  string `json:"scope_kind"`
	ScopeValue string `json:"scope_value"`
	Reason     string `json:"reason"`
	For        string `json:"for"`
	ExpiresAt  string `json:"expires_at"`
}

// apiMuteCreate holds notifications for a scope until a deadline, through the
// SAME store.CreateNotifyMute the CLI's `mute add` and the web banner write. A
// mute lets the job run and only holds the message that would have announced it
// — distinct from a silence, which stops the event ever becoming a job. The
// scope and its value are validated here so a bad scope is a 400, not the 500
// the store's untyped validation error would otherwise map to.
func (s *Server) apiMuteCreate(w http.ResponseWriter, r *http.Request) {
	var req muteCreateRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.badRequest(w, "request body is not valid JSON")
			return
		}
	}
	switch req.ScopeKind {
	case store.ScopeAlert, store.ScopeHost, store.ScopeGlobal:
	default:
		s.badRequest(w, "scope_kind must be alert, host, or global")
		return
	}
	if req.ScopeKind != store.ScopeGlobal && req.ScopeValue == "" {
		s.badRequest(w, "scope_kind "+req.ScopeKind+" needs a scope_value")
		return
	}

	now := s.now()
	expires, ok := s.muteExpiry(w, req, now)
	if !ok {
		return
	}

	m, err := s.store.CreateNotifyMute(r.Context(), store.NotifyMute{
		ScopeKind:  req.ScopeKind,
		ScopeValue: req.ScopeValue,
		Reason:     req.Reason,
		CreatedAt:  now,
		ExpiresAt:  expires,
		CreatedVia: "api",
	})
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, kindMute, wire.NewMuteList([]store.NotifyMute{m})[0])
}

// muteExpiry resolves the mute's deadline from either `for` (a positive
// duration) or `expires_at` (an absolute RFC3339 time in the future), rendering
// the 400 itself and reporting ok=false when neither is usable. Exactly one is
// required; `for` wins if both are set.
func (s *Server) muteExpiry(w http.ResponseWriter, req muteCreateRequest, now time.Time) (time.Time, bool) {
	switch {
	case req.For != "":
		dur, err := time.ParseDuration(req.For)
		if err != nil || dur <= 0 {
			s.badRequest(w, "for must be a positive Go duration (e.g. 2h)")
			return time.Time{}, false
		}
		return now.Add(dur), true
	case req.ExpiresAt != "":
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			s.badRequest(w, "expires_at must be an RFC3339 timestamp")
			return time.Time{}, false
		}
		if !t.After(now) {
			s.badRequest(w, "expires_at must be in the future")
			return time.Time{}, false
		}
		return t.UTC(), true
	default:
		s.badRequest(w, "a mute never expires-never: set for (e.g. 2h) or expires_at")
		return time.Time{}, false
	}
}

// apiMuteDelete lifts a mute before it expires, through the SAME
// store.DeleteNotifyMute the CLI's `mute rm` calls. An unknown id is a 404
// (store.ErrNotFound), never a silent success.
func (s *Server) apiMuteDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.badRequest(w, "mute id must be a positive integer")
		return
	}
	if err := s.store.DeleteNotifyMute(r.Context(), id); err != nil {
		s.apiFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- checks (create / update / enable / disable / rm / probe) ---------------

// checkWriteRequest is the JSON body of a check create or update. The optional
// kind fields are POINTERS so an omitted field is distinguishable from a zero
// value: the CLI refuses a kind-inapplicable flag through cobra's Changed, and
// this is the JSON equivalent.
type checkWriteRequest struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Interval   string   `json:"interval"`
	Targets    []string `json:"targets"`
	Expect     *int     `json:"expect"`
	MinDays    *int     `json:"min_days"`
	ExpectAddr *string  `json:"expect_addr"`
	Op         *string  `json:"op"`
	Value      *float64 `json:"value"`
}

// checkStates loads each check's per-target state rows, the join NewCheckList
// takes.
func (s *Server) checkStates(ctx context.Context, rows []store.Check) (map[int64][]store.CheckState, error) {
	states := make(map[int64][]store.CheckState, len(rows))
	for _, c := range rows {
		st, err := s.store.ListCheckStates(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		states[c.ID] = st
	}
	return states, nil
}

// checkView projects one check with its target state, mirroring the read route.
func (s *Server) checkView(ctx context.Context, c store.Check) (wire.CheckView, error) {
	states, err := s.checkStates(ctx, []store.Check{c})
	if err != nil {
		return wire.CheckView{}, err
	}
	return wire.NewCheckList([]store.Check{c}, states)[0], nil
}

// decodeJSONBody reads an optional JSON body into v; an absent body is left at
// its zero value, a malformed one is a 400 and reports ok=false.
func (s *Server) decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		s.badRequest(w, "request body is not valid JSON")
		return false
	}
	return true
}

// checkWriteFail maps a store write refusal: an operator-input validation is a
// 400 with the doubled internal namespace stripped (mirroring the CLI's
// validationError), everything else keeps its sentinel (duplicate 409,
// not_found 404).
func (s *Server) checkWriteFail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrValidation) {
		msg := strings.TrimPrefix(err.Error(), "store: validation: store: ")
		msg = strings.TrimPrefix(msg, "store: validation: ")
		s.badRequest(w, msg)
		return
	}
	s.apiFail(w, err)
}

// checkTargetsFromRequest parses the name=spec targets and applies the kind's
// optional fields, refusing a field that does not belong to the kind and a
// metric missing its operator or value — the same contract the CLI's
// applyKindFlags and the add form enforce.
func checkTargetsFromRequest(kind string, specs []string, req checkWriteRequest) ([]store.CheckTarget, error) {
	parsed, err := store.ParseCheckTargets(kind, specs, false)
	if err != nil {
		return nil, err
	}
	if len(parsed) == 0 {
		return nil, errors.New("targets must name at least one target")
	}
	switch {
	case req.Expect != nil && kind != store.CheckHTTP:
		return nil, errors.New("expect applies to kind http")
	case req.MinDays != nil && kind != store.CheckTLS:
		return nil, errors.New("min_days applies to kind tls")
	case req.ExpectAddr != nil && kind != store.CheckDNS:
		return nil, errors.New("expect_addr applies to kind dns")
	case req.Op != nil && kind != store.CheckMetric:
		return nil, errors.New("op applies to kind metric")
	case req.Value != nil && kind != store.CheckMetric:
		return nil, errors.New("value applies to kind metric")
	case kind == store.CheckMetric && req.Op == nil:
		return nil, errors.New("op is required for kind metric")
	case kind == store.CheckMetric && req.Value == nil:
		return nil, errors.New("value is required for kind metric")
	}
	var (
		expect     int
		minDays    int
		expectAddr string
		op         string
		value      float64
	)
	if req.Expect != nil {
		expect = *req.Expect
	}
	if req.MinDays != nil {
		minDays = *req.MinDays
	}
	if req.ExpectAddr != nil {
		expectAddr = *req.ExpectAddr
	}
	if req.Op != nil {
		op = *req.Op
	}
	if req.Value != nil {
		value = *req.Value
	}
	for i := range parsed {
		parsed[i].Expect = expect
		parsed[i].MinDays = minDays
		parsed[i].ExpectAddr = expectAddr
		parsed[i].Op = op
		parsed[i].Value = value
	}
	return parsed, nil
}

// checkIntervalSeconds parses a create's required interval, or an update's
// optional one (falling back to the stored cadence when the field is absent).
func (s *Server) checkIntervalSeconds(w http.ResponseWriter, v string, fallbackS int64) (int64, bool) {
	if v == "" {
		if fallbackS <= 0 {
			s.badRequest(w, "interval is required (a Go duration, e.g. 5m)")
			return 0, false
		}
		return fallbackS, true
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		s.badRequest(w, "interval must be a positive Go duration (e.g. 5m)")
		return 0, false
	}
	return int64(d.Seconds()), true
}

// apiCheckCreate registers a check through the SAME store.CreateCheck the CLI
// and the checks page write. The name is in the body; the per-kind target rules
// are the shared ones, so a check created here is a check the sweep can run.
func (s *Server) apiCheckCreate(w http.ResponseWriter, r *http.Request) {
	var req checkWriteRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		s.badRequest(w, "name is required")
		return
	}
	if req.Kind == "" {
		s.badRequest(w, "kind is required")
		return
	}
	intervalS, ok := s.checkIntervalSeconds(w, req.Interval, 0)
	if !ok {
		return
	}
	targets, err := checkTargetsFromRequest(req.Kind, req.Targets, req)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}
	created, err := s.store.CreateCheck(r.Context(), store.NewCheck{
		Name: req.Name, Kind: req.Kind, IntervalS: intervalS, Targets: targets,
	})
	if err != nil {
		s.checkWriteFail(w, err)
		return
	}
	view, err := s.checkView(r.Context(), created)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindCheck, view)
}

// apiCheckUpdate re-points a stored check in place. The kind is fixed; a target
// whose NAME is kept keeps its state row, so the next sweep can flip it and emit
// a real transition.
func (s *Server) apiCheckUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cur, err := s.store.GetCheck(r.Context(), name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	var req checkWriteRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	intervalS, ok := s.checkIntervalSeconds(w, req.Interval, cur.IntervalS)
	if !ok {
		return
	}
	targets, err := checkTargetsFromRequest(cur.Kind, req.Targets, req)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}
	updated, err := s.store.UpdateCheck(r.Context(), name, targets, intervalS)
	if err != nil {
		s.checkWriteFail(w, err)
		return
	}
	view, err := s.checkView(r.Context(), updated)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindCheck, view)
}

func (s *Server) apiCheckEnable(w http.ResponseWriter, r *http.Request) {
	s.apiCheckSetEnabled(w, r, true)
}

func (s *Server) apiCheckDisable(w http.ResponseWriter, r *http.Request) {
	s.apiCheckSetEnabled(w, r, false)
}

// apiCheckSetEnabled flips the stored flag through the SAME
// store.SetCheckEnabled the CLI's enable/disable and the checks page call, and
// returns the check's new projection.
func (s *Server) apiCheckSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	name := r.PathValue("name")
	if err := s.store.SetCheckEnabled(r.Context(), name, enabled); err != nil {
		s.checkWriteFail(w, err)
		return
	}
	c, err := s.store.GetCheck(r.Context(), name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	view, err := s.checkView(r.Context(), c)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindCheck, view)
}

// apiCheckDelete removes a check; the FK cascades its state rows. An unknown
// name is a 404, never a silent success.
func (s *Server) apiCheckDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteCheck(r.Context(), r.PathValue("name")); err != nil {
		s.apiFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiCheckProbe runs a stored check's probes now and returns the per-target
// result. It writes nothing (D3), so it takes the read floor; an unknown check
// is a 404 and an unknown target a 400.
func (s *Server) apiCheckProbe(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := s.store.GetCheck(r.Context(), name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	only := r.URL.Query().Get("target")
	if only != "" && !checkHasTarget(c, only) {
		s.badRequest(w, "check "+name+" has no target named "+strconv.Quote(only))
		return
	}
	view, err := s.probeCheck(r.Context(), c, only)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindCheckProbe, view)
}

func checkHasTarget(c store.Check, name string) bool {
	for _, t := range c.Targets {
		if t.Name == name {
			return true
		}
	}
	return false
}

// probeCheck runs a check's targets with the sweep's options and writes
// nothing. The netgroup is the sweep's stored value when the target has been
// observed, else a live classification of the spec's address.
func (s *Server) probeCheck(ctx context.Context, c store.Check, only string) (wire.CheckProbeView, error) {
	results, err := checkprobe.Run(ctx, s.store, c, only)
	if err != nil {
		return wire.CheckProbeView{}, err
	}
	views := make([]wire.CheckProbeTargetView, 0, len(results))
	for _, r := range results {
		views = append(views, wire.CheckProbeTargetView{
			Target:     r.Name,
			Up:         r.Up,
			DurationMS: r.DurationMS,
			Netgroup:   r.Netgroup,
			Detail:     r.Detail,
		})
	}
	return wire.NewCheckProbeView(c.Name, views), nil
}

// --- netgroups (create / update / rm) ---------------------------------------

// netgroupWriteRequest is the JSON body of a netgroup create or update.
// prefixes is the CLI's comma-separated CIDR list (a `!`-prefixed member is an
// exclusion), parsed by the one store.ParsePrefixList the CLI and the form use.
type netgroupWriteRequest struct {
	Name     string `json:"name"`
	Prefixes string `json:"prefixes"`
}

func netgroupPrefixes(spec string) (store.PrefixList, error) {
	list, err := store.ParsePrefixList(spec)
	if err != nil {
		return store.PrefixList{}, err
	}
	if len(list.Includes) == 0 {
		return store.PrefixList{}, errors.New("prefixes needs at least one include CIDR")
	}
	return list, nil
}

// apiNetgroupCreate registers a named CIDR set through the SAME
// store.CreateNetgroup the CLI and the netgroups page write.
func (s *Server) apiNetgroupCreate(w http.ResponseWriter, r *http.Request) {
	var req netgroupWriteRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		s.badRequest(w, "name is required")
		return
	}
	list, err := netgroupPrefixes(req.Prefixes)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}
	g, err := s.store.CreateNetgroup(r.Context(), store.NewNetgroup{
		Name: req.Name, Prefixes: list.Includes, Excludes: list.Excludes,
	})
	if err != nil {
		s.checkWriteFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindNetgroup, wire.NewNetgroupList([]store.Netgroup{g})[0])
}

// apiNetgroupUpdate replaces a group's set in place, leaving stored check state
// untouched.
func (s *Server) apiNetgroupUpdate(w http.ResponseWriter, r *http.Request) {
	var req netgroupWriteRequest
	if !s.decodeJSONBody(w, r, &req) {
		return
	}
	list, err := netgroupPrefixes(req.Prefixes)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}
	g, err := s.store.UpdateNetgroup(r.Context(), r.PathValue("name"), list.Includes, list.Excludes)
	if err != nil {
		s.checkWriteFail(w, err)
		return
	}
	s.apiSuccess(w, wire.KindNetgroup, wire.NewNetgroupList([]store.Netgroup{g})[0])
}

// apiNetgroupDelete removes a named CIDR set. An unknown name is a 404.
func (s *Server) apiNetgroupDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteNetgroup(r.Context(), r.PathValue("name")); err != nil {
		s.apiFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- host self-service requests ---------------------------------------------
//
// A host-scoped token asks exocomp to run a class of job on itself. The
// request is normalized to a reactive event with the host identity taken from
// the token, never from the body, so it converges on the same rules,
// guardrails, and approval gates as any other event. Scope "host" authorizes
// nothing else.

type hostRequestBody struct {
	Request  string            `json:"request"`
	Reason   string            `json:"reason,omitempty"`
	Status   string            `json:"status,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// hostRequestMaxWait bounds the ?wait=true block. The handler holds the request
// and its connection open until the job terminalises or the deadline passes, so
// an unbounded timeout would pin both indefinitely. An over-cap value is a 400
// naming this maximum, never a silent clamp.
const hostRequestMaxWait = 5 * time.Minute

func (s *Server) apiHostRequest(w http.ResponseWriter, r *http.Request) {
	id, _ := identityOf(r.Context())
	if s.engine == nil {
		s.apiError(w, http.StatusServiceUnavailable,
			wire.NewError(wire.CodeInternal, "host requests are not available on this surface"))
		return
	}
	if id.token.Scope != store.ScopeHostToken || strings.TrimSpace(id.token.Host) == "" {
		s.apiError(w, http.StatusForbidden,
			wire.NewError(wire.CodeForbidden, "this token is not bound to a host"))
		return
	}
	host := id.token.Host

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		s.apiError(w, http.StatusRequestEntityTooLarge,
			wire.NewError(codeInvalidArgument, "body too large"))
		return
	}
	var payload hostRequestBody
	if err := json.Unmarshal(body, &payload); err != nil {
		s.apiError(w, http.StatusBadRequest,
			wire.NewError(codeInvalidArgument, "body is not valid JSON"))
		return
	}
	if strings.TrimSpace(payload.Request) == "" {
		s.apiError(w, http.StatusBadRequest,
			wire.NewError(codeInvalidArgument, "request is required"))
		return
	}
	status := payload.Status
	if status == "" {
		status = reactive.StatusFiring
	}
	if status != reactive.StatusFiring && status != reactive.StatusResolved {
		s.apiError(w, http.StatusBadRequest,
			wire.NewError(codeInvalidArgument, "status must be firing or resolved"))
		return
	}

	wait := false
	timeout := 30 * time.Second
	if wq := r.URL.Query().Get("wait"); wq == "true" || wq == "1" {
		wait = true
		if raw := r.URL.Query().Get("timeout"); raw != "" {
			if d, terr := time.ParseDuration(raw); terr == nil && d > 0 {
				if d > hostRequestMaxWait {
					s.apiError(w, http.StatusBadRequest, wire.NewError(codeInvalidArgument,
						"timeout exceeds the maximum of "+hostRequestMaxWait.String()))
					return
				}
				timeout = d
			}
		}
	}

	labels := map[string]string{}
	if payload.Reason != "" {
		labels["reason"] = payload.Reason
	}
	for k, v := range payload.Metadata {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		labels["meta."+k] = v
	}
	ev := reactive.Event{
		Source:     reactive.SourceHost,
		Kind:       "request",
		Name:       payload.Request,
		Host:       host,
		Instance:   host,
		Status:     status,
		Labels:     labels,
		ReceivedAt: s.now(),
	}
	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
		ev.Fingerprint = key
		ev.DedupKey = reactive.SourceHost + ":" + host + ":fp:" + key
	} else {
		ev.DedupKey = reactive.SourceHost + ":" + payload.Request + ":" + host
	}

	stored, err := s.engine.Ingest(r.Context(), ev, body)
	if err != nil {
		s.log.Error("api: host request ingest", "host", host, "err", err)
		s.apiError(w, http.StatusInternalServerError,
			wire.NewError(wire.CodeInternal, "request failed"))
		return
	}

	// A suppressed duplicate tells the caller nothing about the in-flight
	// job on its own. Look the previous decision up so the retry gets the
	// handle it needs instead of re-posting blindly.
	respJobID := stored.JobID
	if stored.Outcome == store.OutcomeSuppressed && respJobID == nil {
		if prev, perr := s.store.LastEventByDedupKey(r.Context(), stored.DedupKey); perr == nil {
			respJobID = prev.JobID
		}
	}

	resp := map[string]any{
		"event_id": stored.ID,
		"outcome":  string(stored.Outcome),
		"reason":   stored.Reason,
		"job_id":   respJobID,
	}
	var approvalID *int64
	if respJobID != nil {
		if a, aerr := s.store.ApprovalByJob(r.Context(), *respJobID); aerr == nil {
			idv := a.ID
			approvalID = &idv
		}
	}
	resp["approval_id"] = approvalID

	if wait {
		deadline := s.now().Add(timeout)
		if respJobID != nil {
			for {
				select {
				case <-r.Context().Done():
					resp["job_state"] = "pending"
					goto done
				default:
				}
				job, jerr := s.store.GetJob(r.Context(), *respJobID)
				if jerr == nil && (job.State == store.StateSucceeded || job.State == store.StateFailed ||
					job.State == store.StateCancelled || job.State == store.StateInterrupted ||
					job.State == store.StateTimedOut || job.State == store.StateDegraded) {
					resp["job_state"] = string(job.State)
					if job.ExitCode != nil {
						resp["exit_code"] = *job.ExitCode
					}
					break
				}
				if s.now().After(deadline) {
					resp["job_state"] = "pending"
					break
				}
				select {
				case <-r.Context().Done():
					resp["job_state"] = "pending"
					goto done
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	done:
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := wire.WriteSuccess(w, wire.KindHostRequest, resp); err != nil {
		s.log.Error("api: write host request", "err", err)
	}
}
