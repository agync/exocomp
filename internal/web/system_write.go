package web

import (
	"net/http"
	"net/url"
	"strings"

	"exocomp.dev/exocomp/internal/store"
)

// The /system surface is read-only except for one operator control: the public
// URL a phone companion dials and an approval link is rooted at. It is
// non-secret configuration, so it takes the operator gate (writeGuard) the
// other browser writes share, and no re-auth — nothing here mints or reveals a
// secret.

func init() { registerWrite((*Server).registerSystemWrite) }

func (s *Server) registerSystemWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /system/public-url", s.requireAuth(s.handleSetPublicURL))
}

func (s *Server) handleSetPublicURL(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	raw := strings.TrimSpace(r.PostFormValue("url"))
	if !validPublicURL(raw) {
		http.Error(w, "public URL must be an absolute http or https URL", http.StatusBadRequest)
		return
	}
	if err := s.store.PutSetting(r.Context(), store.SettingPublicURL, raw); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: public url set", "by", ident.token.Name)
	s.redirectFlash(w, r, "/system", "public URL set")
}

// validPublicURL accepts only an absolute http(s) URL with a host.
func validPublicURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
