package web

import (
	"net/http"
	"net/url"

	"exocomp.dev/exocomp/internal/secrets"
)

// This file is the shared scaffolding for the browser write surface. Every
// mutating control in the UI is a cookie-session form POST (never /api/v1, which
// is bearer-only and refuses the ambient session — api_write.go). Each resource
// registers its own routes through registerWriteUI so server.go stays the single
// read-surface file, and each handler opens with writeGuard.

// Sealer seals a plaintext value into the envelope for the sensitive write
// surface (secrets add/rotate, and a git source's deploy key). A nil Sealer — a
// web-only process with no open envelope — makes those routes answer "not
// available on this surface", the same fail-closed shape as a nil Decider.
type Sealer interface {
	Seal(name, kind string, pt []byte) (secrets.SealedSecret, error)
}

// writeGuard is the preamble every mutating form handler shares: the caller must
// hold an operator token, the request must be same-origin, and the form must
// parse. It returns the resolved identity, or reports ok=false having already
// written the response.
func (s *Server) writeGuard(w http.ResponseWriter, r *http.Request) (identity, bool) {
	ident, ok := identityOf(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return identity{}, false
	}
	if !canWrite(ident) {
		http.Error(w, "this token may not manage resources", http.StatusForbidden)
		return identity{}, false
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin form post refused", http.StatusForbidden)
		return identity{}, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return identity{}, false
	}
	return ident, true
}

// sameOrigin refuses a state-changing POST a browser reports as cross-site. The
// ambient session cookie does not give this on its own (SameSite=Strict does not
// scope to a port — api_write.go); Sec-Fetch-Site, sent by current browsers on
// every form post, does. It falls back to an Origin/Host compare, and lets a
// request with neither header through to the scope and re-auth checks (a
// non-browser client that cannot be a CSRF vector).
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "cross-site", "same-site":
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	}
	return true
}

// reauth proves the operator at the keyboard still holds the secret their
// session names, not just the ambient cookie — the gate on a sealed or
// credential-minting write. It reuses authenticateTokenGated, so the KDF gate
// and fail-throttle cover this path, and requires the SAME principal as the
// session, so a pasted foreign operator token cannot launder the action's
// attribution.
func (s *Server) reauth(r *http.Request) error {
	ident, ok := identityOf(r.Context())
	if !ok {
		return ErrUnauthenticated
	}
	presented := r.PostFormValue("confirm_token")
	id, _, err := parseToken(presented)
	if err != nil {
		return ErrUnauthenticated
	}
	if id != ident.token.ID {
		return ErrUnauthenticated
	}
	row, err := s.authenticateTokenGated(r, presented, s.now())
	if err != nil {
		return err
	}
	if row.ID != ident.token.ID {
		return ErrUnauthenticated
	}
	return nil
}

// redirectFlash ends a mutating form with a 303 to a GET page carrying a
// one-line result. The message is operator-facing status only — never a secret
// (invariant 4): a value or token is shown once inline, never redirected.
func redirectFlash(w http.ResponseWriter, r *http.Request, path, msg string) {
	http.Redirect(w, r, path+"?flash="+url.QueryEscape(msg), http.StatusSeeOther)
}

// registerWriteUI mounts every resource's write routes. Each resource lane adds
// exactly one call here and defines its own registerXWrite in its own file, so
// server.go and this dispatch list are the only shared edit points.
func (s *Server) registerWriteUI(mux *http.ServeMux) {
	s.registerTokenWrite(mux)
	s.registerSecretWrite(mux)
}
