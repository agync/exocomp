package web

import (
	"context"
	"net/http"
	"strconv"

	"exocomp.dev/exocomp/internal/store"
)

// kitMeta reads the Recovery-kit staleness state (SPEC §7 A3): the date it
// was printed and how many invalidating changes landed since. Stale tints the
// identity strip's meta when a change landed after the last print.
func (s *Server) kitMeta(ctx context.Context) kitMeta {
	m := kitMeta{}
	if v, err := s.store.GetSetting(ctx, store.SettingKitPrintedAt); err == nil {
		m.Printed = v
	}
	if v, err := s.store.GetSetting(ctx, store.SettingKitChanges); err == nil {
		if n, perr := strconv.Atoi(v); perr == nil {
			m.Changes = n
		}
	}
	m.Stale = m.Changes > 0
	return m
}

// handleKit serves the kit modal's Download target: the rendered kit as a raw
// text attachment. The body is KitStructure().Render() only — public data
// (recipients, salt, recipe), never the recovery secret or a sealed value. A
// web-only process holding no envelope fails the route closed to 503, the same
// shape as the Sealer's.
func (s *Server) handleKit(w http.ResponseWriter, _ *http.Request) {
	kit, err := s.deriver.Kit()
	if err != nil {
		http.Error(w, "the recovery kit is not available on this surface", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="recovery-kit.txt"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(kit))
}
