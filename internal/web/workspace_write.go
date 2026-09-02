package web

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// The workspace surface is the browser form face of the same verbs
// api_write.go's apiWorkspacePlan/apiWorkspaceApply call: service.PlanWorkspace
// and service.ApplyWorkspace, through the SAME pure-enqueue Enqueuer
// (AGENTS.md invariant 5). A plan started here carries the "api" trigger, so
// the daemon seals its -out into a consumable artifact and records its policy
// verdict post-run, exactly as the API face's plan does. Workspaces carry no
// secret material of their own, so — like run and schedules — this lane is
// plain operator-scope: no reauth, no confirm_token.

func init() { registerWrite((*Server).registerWorkspaceWrite) }

func (s *Server) registerWorkspaceWrite(mux *http.ServeMux) {
	mux.HandleFunc("GET /workspaces", s.requireAuth(s.handleWorkspaces))
	mux.HandleFunc("GET /workspaces/{name}", s.requireAuth(s.handleWorkspaceDetail))
	mux.HandleFunc("POST /workspaces/{name}/plan", s.requireAuth(s.handleWorkspacePlan))
	mux.HandleFunc("POST /workspaces/{name}/apply", s.requireAuth(s.handleWorkspaceApply))
}

type workspacesVM struct {
	Page       page
	Operator   bool
	Workspaces []workspaceWriteRow
	// Config is an example backend block at the state backend's base address,
	// for the Configure modal's how-to. Its workspace name is a placeholder; a
	// row's Backend config gives the exact block. Empty Address when unbound.
	Config api.BackendBlock
}

// configExampleWorkspace is the placeholder name the Configure modal's example
// backend block carries; PathEscape leaves it intact so the address reads
// cleanly. A row's Backend config carries the real per-workspace block.
const configExampleWorkspace = "WORKSPACE"

// planArtifactRow is one saved plan of a workspace, referenced by artifact id
// only: the apply form posts just the id, and the handler ignores the job for
// applying. Ordering by artifact id IS recency.
type planArtifactRow struct {
	ID int64
}

// workspaceWriteRow is one tofu workspace on the write surface: its name, root
// dir, the newest tfstate serial stored for it (0 means no state has ever
// been pushed), and its backend config block (empty Address when the state
// backend's address is not yet recorded). Plans lists the workspace's saved
// plan artifacts (newest first, bounded), empty for state-only rows that the
// daemon never plans.
type workspaceWriteRow struct {
	Name    string
	TofuDir string
	Serial  int64
	Backend api.BackendBlock
	Plans   []planArtifactRow
}

// StateOnly reports the "state-only" surface distinction: an empty TofuDir
// means exocomp only stores this workspace's state — an external tofu plans
// and applies it. A non-empty TofuDir means exocomp runs it (Plan/Apply
// below).
func (r workspaceWriteRow) StateOnly() bool { return r.TofuDir == "" }

// TypeLabel and TypeClass render the state-only/runs-in-server distinction as
// a labelled chip, never colour-only.
func (r workspaceWriteRow) TypeLabel() string {
	if r.StateOnly() {
		return "state-only"
	}
	return "runs in server"
}

func (r workspaceWriteRow) TypeClass() string {
	if r.StateOnly() {
		return "dim"
	}
	return "ok"
}

// StateLabel and StateClass render the serial as a labelled chip, never
// colour-only: no state pushed yet is dim, a pushed serial is ok.
func (r workspaceWriteRow) StateLabel() string {
	if r.Serial == 0 {
		return "no state yet"
	}
	return "version " + itoa(r.Serial)
}

func (r workspaceWriteRow) StateClass() string {
	if r.Serial == 0 {
		return "dim"
	}
	return "ok"
}

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	ident, _ := identityOf(r.Context())
	vm := workspacesVM{Page: s.pageFor(r, "Workspaces", "workspaces"), Operator: canWrite(ident)}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "ws-configure", Label: "workspace", Text: "Configure"}
	}
	list, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The backend config block needs the state backend's own bound address,
	// recorded the same way the CLI reads it (api.ReadAddress): the file a
	// running `exocomp serve` writes into the data dir. Empty when the backend
	// has never bound (e.g. this process is web-only) — the row then shows a
	// note instead of a block.
	addr, err := api.ReadAddress(filepath.Join(s.dataDir, api.AddressFile))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if addr != "" {
		vm.Config = api.NewBackendBlock(s.deriver, addr, configExampleWorkspace)
	}
	for _, ws := range list {
		row := workspaceWriteRow{Name: ws.Name, TofuDir: ws.TofuDir, Serial: ws.Serial}
		if addr != "" {
			row.Backend = api.NewBackendBlock(s.deriver, addr, ws.Name)
		}
		if !row.StateOnly() && vm.Operator {
			// The Saved plans modal renders only for operators, so readers skip
			// the per-row plan query too — a failure there cannot 500 their page.

			plans, err := s.planArtifactsFor(r.Context(), reactive.WorkspaceRunbook(ws.Name), reactive.PlanArtifactKind, 20)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			for _, p := range plans {
				row.Plans = append(row.Plans, planArtifactRow{ID: p.ID})
			}
		}
		vm.Workspaces = append(vm.Workspaces, row)
	}
	s.render(w, r, http.StatusOK, workspacesList(vm))
}

// handleWorkspacePlan mirrors apiWorkspacePlan (api_write.go): the same
// PlanWorkspace call with the "api" trigger, so the daemon that claims the row
// seals its -out into a consumable artifact and records its policy verdict
// post-run, whichever face asked for the plan.
func (s *Server) handleWorkspacePlan(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	ws, err := s.store.WorkspaceByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no workspace named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	timeout, err := parseOptionalDuration(r, "timeout")
	if err != nil {
		http.Error(w, "timeout: "+err.Error(), http.StatusBadRequest)
		return
	}
	artifacts, err := s.tofuArtifactsDir()
	if err != nil {
		s.fail(w, r, err)
		return
	}

	res, err := s.service().PlanWorkspace(r.Context(), s.enqueuer(), service.PlanInput{
		Workspace:      ws.Name,
		TofuDir:        ws.TofuDir,
		ArtifactsDir:   artifacts,
		RunSecrets:     r.PostFormValue("run_secrets"),
		RunnerName:     r.PostFormValue("runner"),
		IdempotencyKey: idempotencyKey(r, routePlan),
		Timeout:        timeout,
		// The "api" trigger tells the daemon to seal this plan's -out file into
		// a consumable artifact post-run, exactly as a bearer-token plan does —
		// a browser operator and an API caller reach the same sealed-plan path.
		Trigger: service.TriggerAPI,
		Actor:   service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name},
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: workspace plan", "job", res.Job.ID, "workspace", ws.Name, "by", ident.token.Name)
	redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Job.ID, 10),
		"job "+strconv.FormatInt(res.Job.ID, 10)+" enqueued")
}

// handleWorkspaceApply mirrors apiWorkspaceApply (api_write.go:296-391): a
// fresh apply (no plan field) auto-approves and enqueues plainly, unless the
// workspace has enabled plan policies, in which case a fresh apply is refused
// (FreshApplyPolicedError) since there is no plan for a policy to read. A plan
// named by artifact id carries its recorded verdict: a deny is refused, a
// require_approval routes the apply through the SAME approval gate
// handleDecide answers, and a plain saved-plan apply enqueues against the
// sealed plan path (the daemon that claims the row decrypts it for the one run
// that consumes it).
func (s *Server) handleWorkspaceApply(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	ws, err := s.store.WorkspaceByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no workspace named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	timeout, err := parseOptionalDuration(r, "timeout")
	if err != nil {
		http.Error(w, "timeout: "+err.Error(), http.StatusBadRequest)
		return
	}

	svc := s.service()
	var saved *store.Artifact
	var gate service.PlanGate
	if planID := r.PostFormValue("plan"); planID != "" {
		id, perr := strconv.ParseInt(planID, 10, 64)
		if perr != nil || id <= 0 {
			http.Error(w, "plan must be a saved plan artifact id", http.StatusBadRequest)
			return
		}
		art, gerr := s.store.GetArtifact(r.Context(), id)
		if gerr != nil {
			if errors.Is(gerr, store.ErrNotFound) {
				http.Error(w, "no artifact "+planID, http.StatusNotFound)
				return
			}
			s.fail(w, r, gerr)
			return
		}
		if art.Kind != reactive.PlanArtifactKind {
			http.Error(w, "artifact "+planID+" is not a plan", http.StatusBadRequest)
			return
		}
		saved = &art
		// Fail-closed adjudication: a plan on a policied workspace with no
		// recorded verdict is unconsumable, not unpoliced.
		gate, err = svc.PlanGateForPlan(r.Context(), ws.Name, art)
		var mismatch *service.PlanWorkspaceMismatchError
		var unadjudicated *service.PlanUnadjudicatedError
		switch {
		case errors.As(err, &mismatch):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.As(err, &unadjudicated):
			// The API face maps the state-conflict sentinel this unwraps to a 409
			// with its message; an unconsumable plan is a refusal of the asked-for
			// apply, not a server failure.

			http.Error(w, err.Error(), http.StatusConflict)
			return
		case err != nil:
			s.fail(w, r, err)
			return
		}
	}

	res, err := svc.ApplyWorkspace(r.Context(), s.enqueuer(), s.deriver, service.ApplyInput{
		Workspace:       ws.Name,
		WorkspaceSerial: ws.Serial,
		TofuDir:         ws.TofuDir,
		RunSecrets:      r.PostFormValue("run_secrets"),
		RunnerName:      r.PostFormValue("runner"),
		IdempotencyKey:  idempotencyKey(r, routeApply),
		Timeout:         timeout,
		SavedPlan:       saved,
		Gate:            gate,
		Actor:           service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name},
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		if saved == nil {
			return executor.TofuArgv("apply", "-auto-approve"), nil, func() {}, nil
		}
		// Enqueue the apply against the SEALED plan path; the daemon that
		// claims the row decrypts it into a run-private plaintext for the one
		// run that consumes it, so no plaintext plan is left in the shared
		// data dir by this pure-enqueue face.
		return executor.TofuArgv("apply", saved.Path), saved, func() {}, nil
	})

	var denied *service.PlanDeniedError
	var policed *service.FreshApplyPolicedError
	switch {
	case errors.As(err, &denied):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.As(err, &policed):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}

	if res.Gated {
		a := res.Approval
		s.log.Info("web: workspace apply gated", "job", a.JobID, "approval", a.ApprovalID, "by", ident.token.Name)
		redirectFlash(w, r, "/approvals",
			"job "+strconv.FormatInt(a.JobID, 10)+" gated; approval "+strconv.FormatInt(a.ApprovalID, 10)+" pending")
		return
	}
	run := res.Run
	defer run.Release()
	s.log.Info("web: workspace apply", "job", run.Job.ID, "workspace", ws.Name, "by", ident.token.Name)
	redirectFlash(w, r, "/jobs/"+strconv.FormatInt(run.Job.ID, 10),
		"job "+strconv.FormatInt(run.Job.ID, 10)+" enqueued")
}
