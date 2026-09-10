package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
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
		Row: workspaceWriteRow{Name: ws.Name, TofuDir: ws.TofuDir, Serial: ws.Serial},
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
		for i, p := range plans {
			row := planArtifactRow{ID: p.ID, PlanSerial: p.PlanSerial}
			if i == 0 {
				row.Lifecycle = "SAVED"
			} else {
				row.Lifecycle = "SUPERSEDED"
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
