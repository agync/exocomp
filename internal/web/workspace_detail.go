package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// workspaceDetailVM is one workspace's detail page: the identity + state facts
// (type, state, serial, backend address), the Saved plans the row used to show
// in a modal, the exact Backend config block, and the Plan/Apply verbs. This is
// where the row's view items live (R8/v0.6) — the list row keeps only the
// Plan/Apply glyphs.
type workspaceDetailVM struct {
	Page     page
	Operator bool
	Row      workspaceWriteRow
	Config   api.BackendBlock
	// Destroy renders the destroy confirm dialog already open with no JS
	// (?destroy=1), the workspace-level twin of the plan detail's ?launch=1.
	Destroy bool
}

// DestroyKicker is the destroy confirm's eyebrow: DESTROY · WORKSPACE X.
func (vm workspaceDetailVM) DestroyKicker() string {
	return "DESTROY · WORKSPACE " + strings.ToUpper(vm.Row.Name)
}

// handleWorkspaceDetail renders one workspace's detail page. It is the home of
// Saved plans and Backend config (the row's view items moved here); the row
// itself opens it and keeps only the Plan/Apply glyphs.
func (s *Server) handleWorkspaceDetail(w http.ResponseWriter, r *http.Request) {
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
	ident, _ := identityOf(r.Context())
	vm := workspaceDetailVM{
		Page: s.pageFor(r, name, "workspaces"), Operator: canWrite(ident),
		Row: workspaceWriteRow{Name: ws.Name, TofuDir: ws.TofuDir, Serial: ws.Serial, Base: s.basePath()},
		// ?destroy=1 is the JS-off fallback that renders the destroy confirm
		// dialog already open, mirroring the plan detail's ?launch=1.
		Destroy: r.URL.Query().Get("destroy") == "1",
	}
	addr, err := api.ReadAddress(filepath.Join(s.dataDir, api.AddressFile))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if addr != "" {
		vm.Config = api.NewBackendBlock(s.deriver, addr, configExampleWorkspace)
		vm.Row.Backend = api.NewBackendBlock(s.deriver, addr, ws.Name)
	}
	if !vm.Row.StateOnly() && vm.Operator {
		plans, err := s.planArtifactsFor(r.Context(), reactive.WorkspaceRunbook(ws.Name), reactive.PlanArtifactKind, 20)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		now := s.now()
		for _, meta := range dedupedPlanRows(plans) {
			p := meta.Artifact
			row := planArtifactRow{
				ID:         p.ID,
				PlanSerial: p.PlanSerial,
				Lifecycle:  meta.Lifecycle,
				ConsumedBy: meta.ConsumedBy,
			}
			// Stale: the Tofu state has moved past the serial this plan was made
			// against — the chip stays SAVED but the row loses its apply (SPEC §2.2).
			row.Stale = p.PlanSerial > 0 && p.PlanSerial < ws.Serial
			row.Changes, row.Verdict, err = s.planJobSurface(r.Context(), p)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			if p.JobID != 0 {
				if job, jerr := s.store.GetJob(r.Context(), p.JobID); jerr == nil && job.FinishedAt != nil {
					row.Sealed = finishedRel(*job.FinishedAt, now)
				}
			}
			vm.Row.Plans = append(vm.Row.Plans, row)
		}
	}
	s.render(w, r, http.StatusOK, workspaceDetail(vm))
}

// planRowMeta is one deduped saved plan of a workspace: the sealing artifact,
// its lifecycle word, and the consuming apply job when the plan has been
// applied (CONSUMED #job, SPEC §2.2).
type planRowMeta struct {
	Artifact   store.Artifact
	Lifecycle  string // SAVED | SUPERSEDED | CONSUMED
	ConsumedBy int64
}

// dedupedPlanRows collapses a workspace's saved plan artifacts (newest first)
// onto distinct plans by sha256, so the apply's same-SHA re-record never
// doubles the tray count and "N plans" reads honestly. Two rows sharing a
// sha256 are one plan: the older is the seal, the newer — recorded by the
// apply that consumed it — names the consuming job. Among the distinct plans,
// the newest is SAVED, every older one SUPERSEDED, and a consumed one reads
// CONSUMED #job. A consumed plan is never also the latest SAVED one, so the
// single apply affordance (SPEC §2.2) lands on the newest unconsumed plan.
func dedupedPlanRows(plans []store.Artifact) []planRowMeta {
	type group struct {
		seal       store.Artifact
		consumedBy int64
	}
	groups := map[string]*group{}
	for _, p := range plans { // newest first
		g := groups[p.SHA256]
		if g == nil {
			g = &group{seal: p}
			groups[p.SHA256] = g
			continue
		}
		// The seal is the oldest record (a plan must exist before it is
		// applied), so the group's seal is the smallest id.
		if p.ID < g.seal.ID {
			g.seal = p
		}
	}
	// plans is newest first, so the first record of a group that a DIFFERENT
	// job wrote is the newest apply that consumed the plan; it names the
	// consume. A record with no job never consumes anything.
	for _, p := range plans {
		g := groups[p.SHA256]
		if p.JobID != 0 && p.JobID != g.seal.JobID && g.consumedBy == 0 {
			g.consumedBy = p.JobID
		}
	}
	out := make([]planRowMeta, 0, len(groups))
	for _, g := range groups {
		lifecycle := "SUPERSEDED"
		if g.consumedBy != 0 {
			lifecycle = "CONSUMED"
		}
		out = append(out, planRowMeta{
			Artifact:   g.seal,
			Lifecycle:  lifecycle,
			ConsumedBy: g.consumedBy,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Artifact.ID > out[j].Artifact.ID })
	saved := false
	for i := range out {
		if out[i].ConsumedBy != 0 {
			continue
		}
		if !saved {
			out[i].Lifecycle = "SAVED"
			saved = true
		}
	}
	return out
}

// planJobSurface reads the truthful tray facts a plan job recorded as
// append-only events: the plan outcome (the executor's `tofu` payload) and the
// policy verdict (`plan_policy`). A plan with no job — an operator's own file —
// or an outcome/verdict not yet recorded leaves the row's quiet — placeholders
// (SPEC §2.2).
func (s *Server) planJobSurface(ctx context.Context, art store.Artifact) (planChangesRow, planVerdictRow, error) {
	var changes planChangesRow
	var verdict planVerdictRow
	if art.JobID == 0 {
		return changes, verdict, nil
	}
	events, err := s.store.ListJobEvents(ctx, art.JobID)
	if err != nil {
		return changes, verdict, err
	}
	for _, e := range events {
		if e.RawJSON == "" {
			continue
		}
		if outcome, ok := executor.TofuPlanOutcomeFor(e.RawJSON); ok {
			changes = planChangesFromOutcome(outcome)
		}
		var env struct {
			PlanPolicy *struct {
				Action   string `json:"action"`
				Policy   string `json:"policy"`
				Artifact int64  `json:"artifact"`
			} `json:"plan_policy"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &env); err != nil || env.PlanPolicy == nil {
			continue
		}
		if env.PlanPolicy.Artifact != art.ID {
			continue
		}
		switch env.PlanPolicy.Action {
		case "no_match":
			verdict = planVerdictRow{Present: true}
		case store.PolicyDeny:
			verdict = planVerdictRow{Present: true, Action: store.PolicyDeny, Policy: env.PlanPolicy.Policy}
		case store.PolicyRequireApproval:
			verdict = planVerdictRow{Present: true, Action: store.PolicyRequireApproval, Policy: env.PlanPolicy.Policy}
		}
	}
	return changes, verdict, nil
}

// planChangesFromOutcome maps a plan outcome's pending deltas onto the CHANGES
// cell: the create/update/destroy counters, the changed resource addresses, and
// whether the plan recorded drift. Counters follow the shared risk-ordered
// classification (classifyActions); a replace counts as one create + one
// destroy, mirroring what tofu will do.
func planChangesFromOutcome(o executor.TofuPlanOutcome) planChangesRow {
	row := planChangesRow{Present: true, Drift: len(o.Drift) > 0}
	if !o.Changes {
		return row
	}
	addrs := make([]string, 0, len(o.Pending))
	for _, d := range o.Pending {
		addrs = append(addrs, d.Address)
		switch classifyActions(d.Actions) {
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

// handleWorkspaceBackendReveal reveals a workspace's state-backend bearer to an
// operator for the 30s the reveal glyph shows it beside the reference block.
// The bearer is derived on demand (api.WorkspaceToken — never stored) exactly
// as the CLI's `workspace backend --config` derives it. Reveal is a deliberate,
// logged action (SPEC §2.4): the control plane keeps no operator-action ledger,
// so the web server's own log is the audit trail. Copy never returns the value.
func (s *Server) handleWorkspaceBackendReveal(w http.ResponseWriter, r *http.Request) {
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
	addr, err := api.ReadAddress(filepath.Join(s.dataDir, api.AddressFile))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if addr == "" {
		http.Error(w, "no recorded state backend address", http.StatusNotFound)
		return
	}
	token := api.NewBackendBlock(s.deriver, addr, ws.Name).Token
	s.log.Info("web: workspace backend revealed", "workspace", ws.Name, "by", ident.token.Name)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(token))
}
