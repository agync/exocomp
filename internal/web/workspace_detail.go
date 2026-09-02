package web

import (
	"errors"
	"net/http"
	"path/filepath"

	"exocomp.dev/exocomp/internal/api"
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
		for _, p := range plans {
			vm.Row.Plans = append(vm.Row.Plans, planArtifactRow{ID: p.ID})
		}
	}
	s.render(w, r, http.StatusOK, workspaceDetail(vm))
}
