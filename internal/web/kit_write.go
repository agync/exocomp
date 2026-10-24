package web

import (
	"context"
	"net/http"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// The Recovery-kit write surface (SPEC §7 A3): the identity strip's reprint
// glyph stamps the printed date and resets the staleness counter, and the
// three invalidating write verbs — dead-man arm, backup add-target, secret
// rotation — call bumpKitChanges so the kit never goes stale silently. The
// kit itself stays CLI-minted; this surface only records the operator's print.

func init() { registerWrite((*Server).registerKitWrite) }

func (s *Server) registerKitWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /system/kit/reprint", s.requireAuth(s.handleKitReprint))
}

// handleKitReprint records that the operator printed the recovery kit: today's
// date as the printed date and a reset counter. The old page's destruction is
// the operator's, named in the flash.
func (s *Server) handleKitReprint(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if err := s.store.PutSetting(r.Context(), store.SettingKitPrintedAt,
		time.Now().UTC().Format("2006-01-02")); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.PutSetting(r.Context(), store.SettingKitChanges, "0"); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: kit reprint", "by", ident.token.Name)
	s.redirectFlash(w, r, "/system", "recovery kit reprinted — destroy the old page")
}

// bumpKitChanges increments the recovery-kit staleness counter — the honest
// accounting behind the identity strip's "N changes since". Each invalidating
// write verb (arm, add-target, rotate) calls it exactly once; the reprint verb
// resets it. A counter that has not been seeded reads as zero.
func (s *Server) bumpKitChanges(ctx context.Context) {
	s.service().BumpKitChanges(ctx)
}
