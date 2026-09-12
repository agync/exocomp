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
	mux.HandleFunc("GET /workspaces/{name}/plans/{id}", s.requireAuth(s.handleWorkspacePlanDetail))
	mux.HandleFunc("POST /workspaces/{name}/plan", s.requireAuth(s.handleWorkspacePlan))
	mux.HandleFunc("POST /workspaces/{name}/apply", s.requireAuth(s.handleWorkspaceApply))
	mux.HandleFunc("POST /workspaces/{name}/destroy", s.requireAuth(s.handleWorkspaceDestroy))
	mux.HandleFunc("POST /workspaces/{name}/backend/reveal", s.requireAuth(s.handleWorkspaceBackendReveal))
	mux.HandleFunc("POST /workspaces/{name}/unlock", s.requireAuth(s.handleWorkspaceUnlock))
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

// planArtifactRow is one saved plan of a workspace on the detail tray: the
// artifact id (the apply/diff deep-link key), the workspace serial it was made
// against, and the truthful surface facts a tray row reads — LIFECYCLE (SAVED
// for the newest unconsumed plan, SUPERSEDED for older ones, CONSUMED once an
// apply re-recorded the same sealed plan under the applying job's id), whether
// the Tofu state has moved past it (stale → no apply), the plan outcome's
// change counters + resource names + drift, the policy verdict the plan job
// recorded, and when it was sealed. Ordering by artifact id IS recency: the
// first row is the latest SAVED plan.
type planArtifactRow struct {
	ID         int64
	PlanSerial int64
	Lifecycle  string // "SAVED" | "SUPERSEDED" | "CONSUMED"
	// ConsumedBy is the apply job that consumed the plan, 0 until an apply
	// re-recorded it; the LIFECYCLE cell renders the job as a link.
	ConsumedBy int64
	Stale      bool
	Sealed     string
	Changes    planChangesRow
	Verdict    planVerdictRow
}

// Applicable reports the single-apply rule (SPEC §2.2, invariant 7): only the
// latest SAVED plan the Tofu state has not moved past carries an apply
// affordance. Every other row — SUPERSEDED, CONSUMED, or SAVED-but-stale —
// reads only.
func (p planArtifactRow) Applicable() bool { return p.Lifecycle == "SAVED" && !p.Stale }

// LifecycleClass renders the LIFECYCLE chip's signal: accent for the one SAVED
// plan, dim for SUPERSEDED history, ok for a CONSUMED plan whose apply ran.
// Never colour-only (the word is always shown).
func (p planArtifactRow) LifecycleClass() string {
	switch p.Lifecycle {
	case "CONSUMED":
		return "ok"
	case "SAVED":
		return "run"
	default:
		return "dim"
	}
}

// planChangesRow is the CHANGES cell of a plan tray row: the pending-change
// counters and resource addresses the plan's outcome recorded, and whether it
// reported drift. All values-free — the plan outcome never carries attribute
// values (SPEC §2.2).
type planChangesRow struct {
	Present   bool
	Create    int
	Update    int
	Destroy   int
	Resources string
	Drift     bool
}

// planVerdictRow is the policy verdict the plan's job recorded. Present=false
// (no record) renders a quiet —; a no-match adjudication reads "no policy
// matched"; deny/require-approval name the policy that decided (SPEC §2.2).
type planVerdictRow struct {
	Present bool
	Action  string // store.PolicyDeny | store.PolicyRequireApproval, or "" for no match
	Policy  string
}

func (v planVerdictRow) Label() string {
	if !v.Present {
		return ""
	}
	switch v.Action {
	case store.PolicyDeny:
		return "deny · " + v.Policy
	case store.PolicyRequireApproval:
		return "require-approval · pending"
	default:
		return "no policy matched"
	}
}

func (v planVerdictRow) Class() string {
	if !v.Present {
		return "muted"
	}
	switch v.Action {
	case store.PolicyDeny:
		return "fail"
	case store.PolicyRequireApproval:
		return "wait"
	default:
		return "muted"
	}
}

// workspaceWriteRow is one tofu workspace on the write surface: its name, root
// dir, the newest tfstate serial stored for it (0 means no state has ever
// been pushed), and its backend config block (empty Address when the state
// backend's address is not yet recorded). Plans lists the workspace's saved
// plan artifacts (newest first, bounded), empty for state-only rows that the
// daemon never plans. The T7 list columns — POSTURE meta, the DRIFT chip, and
// the TOFU STATE facts — ride the same row; the detail page reads the subset
// it needs.
type workspaceWriteRow struct {
	Name    string
	TofuDir string
	Serial  int64
	Backend api.BackendBlock
	Plans   []planArtifactRow
	// Base is the surface's origin sub-path ("" at the root), so the row's
	// Go-built hrefs re-prefix the same way every other URL site does.
	Base string
	// LastJob is the POSTURE meta for a runs-in-server row ("last #20 ·
	// 16:57", or "no runs yet"); state-only rows read the fixed "external
	// tofu" instead. Empty until workspaceListRows fills it.
	LastJob string
	// ApprovalID is the open drift approval naming this workspace, 0 when
	// none — the PRESENT chip's meta links it.
	ApprovalID int64
	// Locked carries the held tflock (DRIFT LOCKED): who holds it and since
	// when, rendered in the TOFU STATE meta with the force-unlock verb.
	Locked     bool
	LockHolder string
	LockSince  string
	// SerialCount is the stored tfstate count; StateSize the newest body's
	// human size. StateWrittenAt/StateWrittenBy name the newest serial's
	// writer (0 = an external tool wrote it). ExternalWrites counts the
	// serials no job wrote, ExternalSerial the first one's serial.
	SerialCount    int
	StateSize      string
	StateWrittenAt string
	StateWrittenBy int64
	ExternalWrites int
	ExternalSerial int64
}

// StateOnly reports the "state-only" surface distinction: an empty TofuDir
// means exocomp only stores this workspace's state — an external tofu plans
// and applies it. A non-empty TofuDir means exocomp runs it (Plan/Apply
// below).
func (r workspaceWriteRow) StateOnly() bool { return r.TofuDir == "" }

// TypeLabel and TypeClass render the state-only/runs-in-server distinction as
// a labelled chip, never colour-only. The list's POSTURE column reads the same
// word through PostureLabel.
func (r workspaceWriteRow) TypeLabel() string {
	if r.StateOnly() {
		return "state-only"
	}
	return "runs-in-server"
}

func (r workspaceWriteRow) TypeClass() string {
	if r.StateOnly() {
		return "dim"
	}
	return "ok"
}

// PostureLabel and PostureMeta render the list's POSTURE column (T7): the
// posture word and its one-line meta — the newest job for a runs-in-server
// row, the fixed "external tofu" for a state-only one.
func (r workspaceWriteRow) PostureLabel() string {
	if r.StateOnly() {
		return "state-only"
	}
	return "runs-in-server"
}

func (r workspaceWriteRow) PostureMeta() string {
	if r.StateOnly() {
		return "external tofu"
	}
	return r.LastJob
}

// DriftLabel and DriftClass render the DRIFT column's chip (T7): NONE in ok
// when nothing is waiting, PRESENT in wait when an open approval names the
// workspace, LOCKED in wait when a tflock holds it. A held lock wins — an
// apply that cannot start is the stronger fact.
func (r workspaceWriteRow) DriftLabel() string {
	if r.Locked {
		return "LOCKED"
	}
	if r.ApprovalID > 0 {
		return "PRESENT"
	}
	return "NONE"
}

func (r workspaceWriteRow) DriftClass() string {
	if r.Locked || r.ApprovalID > 0 {
		return "wait"
	}
	return "ok"
}

// HasState reports whether the workspace holds any stored state.
func (r workspaceWriteRow) HasState() bool { return r.Serial > 0 }

// StateLine renders the TOFU STATE column's first line: "serial 10 · 10
// serials · 4.1 KB", or "no state yet" when nothing has been pushed.
func (r workspaceWriteRow) StateLine() string {
	if !r.HasState() {
		return "no state yet"
	}
	return "serial " + itoa(r.Serial) + " · " + itoa(int64(r.SerialCount)) + " serials · " + r.StateSize
}

// StateMeta renders the TOFU STATE column's second line: it names the newest
// serial's writer (job #id, or outside exocomp when an external tool wrote it)
// and counts the external writes, naming the first one's serial. A held lock
// replaces this line with LockLine + the force-unlock verb; a workspace with
// no state has no meta.
func (r workspaceWriteRow) StateMeta() string {
	if !r.HasState() {
		return ""
	}
	line := "written " + r.StateWrittenAt
	if r.StateWrittenBy > 0 {
		line += " by job #" + itoa(r.StateWrittenBy)
	} else {
		line += " · outside exocomp"
	}
	if r.ExternalWrites == 1 {
		line += " · 1 external write (serial " + itoa(r.ExternalSerial) + ")"
	} else if r.ExternalWrites > 1 {
		line += " · " + itoa(int64(r.ExternalWrites)) + " external writes (serial " + itoa(r.ExternalSerial) + ")"
	}
	return line
}

// LockLine is the LOCKED row's TOFU STATE meta: who holds the tflock and since
// when, the line the force-unlock verb ends.
func (r workspaceWriteRow) LockLine() string {
	return "lock held by " + r.LockHolder + " since " + r.LockSince
}

// ApplicablePlan reports whether the workspace holds a plan the list's apply
// gate would offer: the latest SAVED plan whose serial equals the Tofu state.
// The list fills only the plan facts applicability needs, so this is the same
// honest gate the detail tray's planArtifactRow.Applicable expresses.
func (r workspaceWriteRow) ApplicablePlan() bool {
	for _, p := range r.Plans {
		if p.Applicable() {
			return true
		}
	}
	return false
}

// PlanMeta renders the TOFU STATE cell's second line for a runs-in-server row:
// "no saved plan" when no saved plan is applicable (the apply glyph is off),
// else the ordinary writer meta. A state-only workspace never plans, so its
// writer meta stands.
func (r workspaceWriteRow) PlanMeta() string {
	if r.StateOnly() || r.ApplicablePlan() {
		return r.StateMeta()
	}
	return "no saved plan"
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
	// The DRIFT chip and its meta come from the pending approvals (an open one
	// names a workspace drifting) — the same ListApprovals rows the dashboard
	// card and the /approvals list read, so the surfaces never disagree.
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
	rows, err := s.workspaceListRows(r.Context(), pending)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, row := range rows {
		if addr != "" {
			row.Backend = api.NewBackendBlock(s.deriver, addr, row.Name)
		}
		if !row.StateOnly() && vm.Operator {
			// The Saved plans modal renders only for operators, so readers skip
			// the per-row plan query too — a failure there cannot 500 their page.

			plans, err := s.planArtifactsFor(r.Context(), reactive.WorkspaceRunbook(row.Name), reactive.PlanArtifactKind, 20)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			// Fill the plan facts the list's apply gate reads — lifecycle,
			// serial, staleness — through the SAME dedupe/lifecycle reduction
			// the detail tray uses, so the list never offers an apply the tray
			// would refuse (SPEC §4, fold SF2).
			for _, meta := range dedupedPlanRows(plans) {
				p := meta.Artifact
				row.Plans = append(row.Plans, planArtifactRow{
					ID:         p.ID,
					PlanSerial: p.PlanSerial,
					Lifecycle:  meta.Lifecycle,
					ConsumedBy: meta.ConsumedBy,
					Stale:      p.PlanSerial > 0 && p.PlanSerial < row.Serial,
				})
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
	s.redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Job.ID, 10),
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
		s.redirectFlash(w, r, "/approvals",
			"job "+strconv.FormatInt(a.JobID, 10)+" gated; approval "+strconv.FormatInt(a.ApprovalID, 10)+" pending")
		return
	}
	run := res.Run
	defer run.Release()
	s.log.Info("web: workspace apply", "job", run.Job.ID, "workspace", ws.Name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/jobs/"+strconv.FormatInt(run.Job.ID, 10),
		"job "+strconv.FormatInt(run.Job.ID, 10)+" enqueued")
}

// handleWorkspaceDestroy is the browser face of the destroy verb: the same
// DestroyWorkspace pure-enqueue the /api/v1 route calls, behind the confirm
// dialog's mandatory confirmed=1 field. It mirrors handleWorkspaceApply's
// shape — writeGuard, workspace lookup, optional timeout, redirect to the job —
// so a destroy started here and one started by a bearer token reach the same
// queued row (invariant 5). The confirm guard is the dialog's, the same hidden
// field every destructive form in the UI posts; a confirmed-less destroy is
// refused before anything is enqueued.
func (s *Server) handleWorkspaceDestroy(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if !r.Form.Has("confirmed") {
		http.Error(w, "destroy requires confirmation", http.StatusBadRequest)
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

	res, err := s.service().DestroyWorkspace(r.Context(), s.enqueuer(), service.DestroyInput{
		Workspace:      ws.Name,
		TofuDir:        ws.TofuDir,
		RunSecrets:     r.PostFormValue("run_secrets"),
		RunnerName:     r.PostFormValue("runner"),
		IdempotencyKey: idempotencyKey(r, routeDestroy),
		Timeout:        timeout,
		Actor:          service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name},
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: workspace destroy", "job", res.Job.ID, "workspace", ws.Name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Job.ID, 10),
		"job "+strconv.FormatInt(res.Job.ID, 10)+" enqueued")
}

// handleWorkspaceUnlock force-releases a workspace's state lock from the list
// row's LOCKED meta (SPEC §5): the same audited verb as `exocomp workspace
// unlock` — a live job's lock is refused (the system working, not a leftover),
// the break is recorded on the job the lock named (or the workspace's newest),
// and the list returns to the ordinary state.
func (s *Server) handleWorkspaceUnlock(w http.ResponseWriter, r *http.Request) {
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
	held, err := s.store.GetTFLock(r.Context(), ws.ID)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, name+" is not locked", http.StatusConflict)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if held.JobID != nil {
		if owner, err := s.store.GetJob(r.Context(), *held.JobID); err == nil && !owner.State.Terminal() {
			http.Error(w, "job "+itoa64(owner.ID)+" is still "+string(owner.State)+"; cancel it first", http.StatusConflict)
			return
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, err)
			return
		}
	}
	broken, err := s.store.ForceReleaseTFLock(r.Context(), ws.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: workspace force-unlock", "workspace", ws.Name, "lock", broken.LockID, "by", ident.token.Name)
	if jobID, err := s.unlockAuditJob(r.Context(), ws, broken); err != nil {
		s.fail(w, r, err)
		return
	} else if jobID > 0 {
		if _, err := s.store.AppendJobEvent(r.Context(), store.JobEvent{
			JobID: jobID,
			Level: store.LevelSystem,
			Msg:   "force-unlocked tofu state: lock " + broken.LockID + ", " + broken.Operation + " by " + broken.Who + " since " + broken.CreatedAt.Format("2006-01-02 15:04:05Z"),
		}); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.redirectFlash(w, r, "/workspaces", name+" unlocked")
}

// unlockAuditJob picks the job an unlock's audit event belongs on: the job the
// lock named when it still exists, else the workspace's newest job, else 0 when
// the workspace has never run — the same rule as the CLI's workspace unlock.
func (s *Server) unlockAuditJob(ctx context.Context, ws store.Workspace, broken store.TFLock) (int64, error) {
	if broken.JobID != nil {
		if _, err := s.store.GetJob(ctx, *broken.JobID); err == nil {
			return *broken.JobID, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
	}
	jobs, err := s.store.ListJobs(ctx, store.JobFilter{Runbook: service.WorkspaceRunbook(ws.Name)})
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, job := range jobs {
		if job.ID > latest {
			latest = job.ID
		}
	}
	return latest, nil
}
