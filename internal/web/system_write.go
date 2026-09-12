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
// secret. The validPublicURL guard (m70) is the boundary: its refusal is a
// field refusal, rendered on the surface with human copy at 422 (SPEC §7 A7),
// never a flash.

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
	form := pubFormVM{Value: raw}
	if !validPublicURL(raw) {
		s.log.Info("web: public url refused", "url", raw, "by", ident.token.Name)
		form.ErrMsg = publicURLError(raw)
		s.pubFormRefusal(w, r, &form)
		return
	}
	if err := s.store.PutSetting(r.Context(), store.SettingPublicURL, raw); err != nil {
		s.log.Error("web: public url set failed", "err", err.Error())
		form.ErrForm = pubSubmitRefusal
		s.pubFormRefusal(w, r, &form)
		return
	}
	s.log.Info("web: public url set", "by", ident.token.Name)
	s.redirectFlash(w, r, "/system", "public URL set")
}

// pubFormRefusal re-renders /system at 422 with the Change form's refusal on
// the surface — the JS-off path, and the fragment body a JS submit swaps in
// place. Refusals never flash: flash is success-only (write.go).
func (s *Server) pubFormRefusal(w http.ResponseWriter, r *http.Request, form *pubFormVM) {
	vm, err := s.systemVMFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.PubForm = form
	s.render(w, r, http.StatusUnprocessableEntity, systemPage(vm))
}

// validPublicURL accepts only an absolute http(s) URL with a host, rooted at
// the origin: the reactive ingress serves /hook and /approval at the root with
// no path support (internal/reactive/token.go, ingress.go), so a public-url
// carrying a path would emit decision links nothing serves.
func validPublicURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		(u.Path == "" || u.Path == "/")
}

// publicURLError explains why a public URL was refused, in the field's terms
// (forms/SPEC.md words table): a well-formed absolute URL that fails
// validation can only fail on its path; a malformed one is told apart so the
// copy names the actual problem. The empty case gets the words-table required
// shape.
func publicURLError(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "Give the public URL."
	}
	u, err := url.Parse(raw)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return "public URL must be at the origin root — the ingress serves /hook and /approval links there, so a path would point them at nothing"
	}
	return "Give the public URL as a full address — like https://exocomp.example.ts.net"
}

// pubSubmitRefusal is the status line's whole-submit failure (forms/SPEC.md):
// the setting write is the only daemon interaction, and a failure there is
// told on the 20px line with Retry, never a field.
const pubSubmitRefusal = "Couldn't update the public URL — nothing was saved."
