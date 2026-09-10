package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// planDetailVM is the plan-detail page (SPEC §4): breadcrumb, one card with the
// lifecycle chips, the +N ~N −N verdict, the structured diff parsed from the
// sealed plan, and the apply confirm dialog that repeats the diff and asks.
// The counters and the diff come from the SAME classifyPlan the approval
// surface and /api/v1 use, so a value the phone redacts is redacted here too.
// Lifecycle (SAVED · SUPERSEDED · CONSUMED) comes from the deduped tray view;
// Stale is a refusal, never a disabled button (invariant 7).
type planDetailVM struct {
	Page       page
	Operator   bool
	CanPlan    bool
	Workspace  string
	Serial     int64
	ID         int64
	PlanSerial int64
	JobID      int64
	Lifecycle  string
	ConsumedBy int64
	Stale      bool
	Latest     bool
	HasPlan    bool
	Drift      bool
	Recorded   string
	Prose      string
	Resources  []planResource
	Destroy    bool
	Launch     bool
	Runner     string
	Changes    planChangesRow
	Verdict    planVerdictRow
}

// handleWorkspacePlanDetail renders one saved plan's page, making the tray's
// diff/play deep-links live (SPEC §4, landing step 6). The plan must belong to
// this workspace's runbook — another workspace's sealed plan under this URL
// would misname the state it was made against. The diff is gated by the same
// mayApprove floor the /api/v1 plan diff uses; a readonly session gets the
// page chrome and a notice, never the resource values.
func (s *Server) handleWorkspacePlanDetail(w http.ResponseWriter, r *http.Request) {
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
	id, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil || id <= 0 {
		http.Error(w, "plan id must be a positive integer", http.StatusBadRequest)
		return
	}
	art, err := s.store.GetArtifact(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no plan "+r.PathValue("id"), http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	if art.Kind != reactive.PlanArtifactKind {
		http.Error(w, "artifact "+itoa(id)+" is not a plan", http.StatusNotFound)
		return
	}
	if art.JobID != 0 {
		job, jerr := s.store.GetJob(r.Context(), art.JobID)
		if jerr != nil {
			s.fail(w, r, jerr)
			return
		}
		if job.Runbook != reactive.WorkspaceRunbook(name) {
			http.Error(w, "no plan "+itoa(id)+" on workspace "+name, http.StatusNotFound)
			return
		}
	}

	ident, _ := identityOf(r.Context())
	vm := planDetailVM{
		Page:       s.pageFor(r, "plan #"+itoa(id), "workspaces"),
		Operator:   canWrite(ident),
		CanPlan:    canWrite(ident) || mayApprove(ident),
		Workspace:  name,
		Serial:     ws.Serial,
		ID:         id,
		PlanSerial: art.PlanSerial,
		JobID:      art.JobID,
		Stale:      art.PlanSerial > 0 && art.PlanSerial < ws.Serial,
		// ?launch=1 is the JS-off fallback that renders the confirm dialog
		// already open; ?destroy=1 opens the same dialog in DESTROY mode.
		Launch:  r.URL.Query().Get("launch") == "1" || r.URL.Query().Get("destroy") == "1",
		Destroy: r.URL.Query().Get("destroy") == "1",
	}

	// Lifecycle comes from the deduped tray view of this workspace's plans, so
	// the page and the tray read one word. A plan missing from the tray (past
	// the bounded list) reads SAVED with no LATEST chip.
	if plans, perr := s.planArtifactsFor(r.Context(), reactive.WorkspaceRunbook(name), reactive.PlanArtifactKind, 20); perr == nil {
		for _, meta := range dedupedPlanRows(plans) {
			if meta.Artifact.ID == id {
				vm.Lifecycle = meta.Lifecycle
				vm.ConsumedBy = meta.ConsumedBy
				break
			}
		}
	}
	if vm.Lifecycle == "" {
		vm.Lifecycle = "SAVED"
	}
	vm.Latest = vm.Lifecycle == "SAVED" && !vm.Stale

	// The app bar's verbs (SPEC §4): Plan text opens the plan job, the accent
	// apply glyph opens the confirm (JS reloads to ?launch=1, JS-off lands on
	// it directly).
	if vm.CanPlan {
		if vm.JobID != 0 {
			vm.Page.Actions = append(vm.Page.Actions, pageAction{
				Href: "/jobs/" + itoa(vm.JobID), Label: "Plan text", Glyph: glyphDocument,
			})
		}
		vm.Page.Actions = append(vm.Page.Actions, pageAction{
			Href: "?launch=1", Label: "Apply plan #" + itoa(id), Kind: "accent", Glyph: glyphApply,
		})
	}

	if art.JobID != 0 {
		if job, jerr := s.store.GetJob(r.Context(), art.JobID); jerr == nil {
			vm.Runner = job.Args.RunnerName
			if job.FinishedAt != nil {
				vm.Recorded = "against serial " + itoa64(art.PlanSerial) + " · recorded " + utcStamp(*job.FinishedAt)
			} else {
				vm.Recorded = "against serial " + itoa64(art.PlanSerial)
			}
			if changes, verdict, verr := s.planJobSurface(r.Context(), art); verr == nil {
				vm.Verdict = verdict
				vm.Drift = changes.Drift
			}
		}
	}
	if vm.Recorded == "" {
		vm.Recorded = "against serial " + itoa64(art.PlanSerial)
	}
	if vm.Runner == "" {
		vm.Runner = "daemon"
	}

	if vm.CanPlan && s.plans != nil {
		blob, oerr := s.planShowBlob(r, art)
		if oerr == nil {
			resources, ok := classifyPlan(blob)
			if ok {
				vm.HasPlan = true
				vm.Resources = resources
				vm.Changes = planChangesFromResources(resources)
			} else {
				s.log.Warn("web: plan detail parse", "artifact", id)
			}
		} else {
			s.log.Warn("web: plan detail open sealed plan", "artifact", id, "err", oerr)
		}
	}
	vm.Prose = s.planProse(vm)

	s.render(w, r, http.StatusOK, planDetailPage(vm))
}

// planChangesFromResources tallies a classified plan's resources into the
// +N ~N −N verdict of this page (SPEC §4): a replace counts as one create and
// one destroy, mirroring what tofu will do — the same mapping the tray's
// outcome-derived CHANGES cell uses.
func planChangesFromResources(resources []planResource) planChangesRow {
	row := planChangesRow{Present: true}
	addrs := make([]string, 0, len(resources))
	for _, rc := range resources {
		addrs = append(addrs, rc.Address)
		switch rc.Action {
		case "create":
			row.Create++
		case "update":
			row.Update++
		case "replace":
			row.Create++
			row.Destroy++
		case "delete":
			row.Destroy++
		}
	}
	row.Resources = strings.Join(addrs, ", ")
	return row
}

// planProse is the one-sentence page lead (SPEC §4): it names the drift finding
// when the plan recorded one, else states the change count plainly. It is
// descriptive only — the counters and the diff are the verdict.
func (s *Server) planProse(vm planDetailVM) string {
	if vm.Drift {
		return fmt.Sprintf("The plan recorded drift — the workspace left the declared state outside exocomp (serial %d). Applying restores the declared state; nothing is created or destroyed.", vm.PlanSerial)
	}
	if !vm.Changes.Present || vm.Changes.Create+vm.Changes.Update+vm.Changes.Destroy == 0 {
		return "This plan changes nothing — the workspace matches its declared state."
	}
	return fmt.Sprintf("This plan changes %d resource(s) on %s: %s.",
		vm.Changes.Create+vm.Changes.Update+vm.Changes.Destroy, vm.Workspace, vm.Changes.Resources)
}

// DestroyResources are the classified rows that tear something down, rendered
// first in their own fail block (SPEC §4, the phone approval contract).
func (vm planDetailVM) DestroyResources() []planResource {
	var out []planResource
	for _, rc := range vm.Resources {
		if rc.Action == "delete" || rc.Action == "replace" {
			out = append(out, rc)
		}
	}
	return out
}

// ChangeResources are the rows that create or update, rendered under the
// ACTION · RESOURCE · ATTRIBUTES header after the destroy block.
func (vm planDetailVM) ChangeResources() []planResource {
	var out []planResource
	for _, rc := range vm.Resources {
		if rc.Action == "delete" || rc.Action == "replace" {
			continue
		}
		out = append(out, rc)
	}
	return out
}

// Kicker is the confirm dialog's eyebrow: APPLY PLAN #n · WORKSPACE X, or
// DESTROY · WORKSPACE X when the confirm was opened for a destroy.
func (vm planDetailVM) Kicker() string {
	if vm.Destroy {
		return "DESTROY · WORKSPACE " + strings.ToUpper(vm.Workspace)
	}
	return fmt.Sprintf("APPLY PLAN #%d · WORKSPACE %s", vm.ID, strings.ToUpper(vm.Workspace))
}

// ConfirmTitle names the ask: "Change N resource(s) on <ws>", or "Destroy N
// resource(s) on <ws>" in destroy mode.
func (vm planDetailVM) ConfirmTitle() string {
	if vm.Destroy {
		return fmt.Sprintf("Destroy %d resource(s) on %s", vm.Changes.Destroy, vm.Workspace)
	}
	return fmt.Sprintf("Change %d resource(s) on %s", vm.Changes.Create+vm.Changes.Update+vm.Changes.Destroy, vm.Workspace)
}

// SummaryLine is the fact grid's Summary row and the confirm's meta line:
// "{n} update · {n} create · {n} destroy" (copy.txt).
func (vm planDetailVM) SummaryLine() string {
	return fmt.Sprintf("%d update · %d create · %d destroy",
		vm.Changes.Update, vm.Changes.Create, vm.Changes.Destroy)
}

// AgainstLine is the confirm's Against fact: "serial N · no drift", or the
// refusal "REFUSED · Tofu state moved to serial N" once the state moved past
// the plan (invariant 7 — the refusal is the copy, the button stays live).
func (vm planDetailVM) AgainstLine() string {
	if vm.Stale {
		return fmt.Sprintf("REFUSED · Tofu state moved to serial %d", vm.Serial)
	}
	return fmt.Sprintf("serial %d · no drift", vm.PlanSerial)
}

func (vm planDetailVM) AgainstClass() string {
	if vm.Stale {
		return "fail"
	}
	return "muted"
}

// PolicyLabel and PolicyClass render the confirm's Policy fact: the recorded
// verdict, or "none matched · no approval needed" when the plan carries none.
func (vm planDetailVM) PolicyLabel() string {
	switch vm.Verdict.Action {
	case store.PolicyDeny:
		return "DENIED · " + vm.Verdict.Policy
	case store.PolicyRequireApproval:
		return "require-approval · " + vm.Verdict.Policy
	default:
		return "none matched · no approval needed"
	}
}

func (vm planDetailVM) PolicyClass() string {
	switch vm.Verdict.Action {
	case store.PolicyDeny:
		return "fail"
	case store.PolicyRequireApproval:
		return "wait"
	default:
		return "muted"
	}
}

// Denied reports the deny verdict: the confirm shows the refusal and NO
// primary — Cancel only (SPEC §4).
func (vm planDetailVM) Denied() bool { return vm.Verdict.Action == store.PolicyDeny }

// RequiresApproval reports the require-approval verdict: the primary reads
// "Queue apply — awaits approval" and the job lands awaiting approval.
func (vm planDetailVM) RequiresApproval() bool {
	return vm.Verdict.Action == store.PolicyRequireApproval
}

// ConfirmPrimary is the confirm dialog's submit label. Stale plans re-plan —
// never a disabled button (invariant 7); deny has no primary.
func (vm planDetailVM) ConfirmPrimary() string {
	if vm.Stale {
		return "Plan again"
	}
	if vm.RequiresApproval() {
		return "Queue apply — awaits approval"
	}
	if vm.Destroy {
		return "Queue destroy — " + vm.Workspace
	}
	return "Queue apply — " + vm.Workspace + " @ plan #" + itoa(vm.ID)
}

// ConfirmForm is where the confirm's primary posts: the same workspace apply
// verb the tray's play glyph used to reach, which refuses a deny and routes a
// require-approval through the gate. A stale plan posts the Plan verb instead.
func (vm planDetailVM) ConfirmForm() string {
	if vm.Stale {
		return "/workspaces/" + vm.Workspace + "/plan"
	}
	return "/workspaces/" + vm.Workspace + "/apply"
}

// RingClass is the card's dominant action colour: fail when anything tears
// down, wait when something updates, ok for create-only (SPEC §4).
func (vm planDetailVM) RingClass() string {
	if vm.Changes.Destroy > 0 {
		return "ring-fail"
	}
	if vm.Changes.Update > 0 {
		return "ring-wait"
	}
	if vm.Changes.Create > 0 {
		return "ring-ok"
	}
	return "ring-dim"
}
