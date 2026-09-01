package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// Tokens are the one credential the network surface accepts. Minting or rotating
// one produces a secret, so those routes are gated behind reauth (proof the
// operator at the keyboard holds their own token, not just the ambient cookie).
// A token list is operator-only. The plaintext of a new or rotated secret is
// shown exactly once, inline — never redirected, flashed, or logged.

func init() { registerWrite((*Server).registerTokenWrite) }

func (s *Server) registerTokenWrite(mux *http.ServeMux) {
	mux.HandleFunc("GET /tokens", s.requireAuth(s.handleTokens))
	mux.HandleFunc("POST /tokens/new", s.requireAuth(s.handleTokenNew))
	mux.HandleFunc("POST /tokens/{name}/rotate", s.requireAuth(s.handleTokenRotate))
	mux.HandleFunc("POST /tokens/{name}/revoke", s.requireAuth(s.handleTokenRevoke))
}

type tokensVM struct {
	Page     page
	Operator bool
	Tokens   []tokenRow
}

type tokenRow struct {
	Name, Scope, Prefix, Expires, LastUsed, State string
	Self                                          bool
}

// tokenResultVM shows a freshly minted or rotated secret once. Secret is the
// only place a token plaintext exists on this surface. BaseURL and QRSVG carry
// the device-pairing view: the QR encodes the same token, so it is as sensitive
// as Secret and equally never logged or persisted past this render.
type tokenResultVM struct {
	Page                                 page
	Rotated                              bool
	Name, Scope, Secret, Expires, Finger string
	BaseURL                              string
	QRSVG                                string
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	ident, _ := identityOf(r.Context())
	vm := tokensVM{Page: s.pageFor(r, "Tokens", "tokens"), Operator: canWrite(ident)}
	if vm.Operator {
		rows, err := s.store.ListTokens(r.Context(), false)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		now := s.now()
		for _, t := range rows {
			vm.Tokens = append(vm.Tokens, tokenRow{
				Name: t.Name, Scope: t.Scope, Prefix: t.Prefix,
				Expires: stampOrDashWeb(t.ExpiresAt), LastUsed: stampOrDashWeb(t.LastUsedAt),
				State: tokenStateWeb(t, now), Self: t.Name == ident.token.Name,
			})
		}
	}
	s.render(w, r, http.StatusOK, tokensPage(vm))
}

func (s *Server) handleTokenNew(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if err := s.reauth(r); err != nil {
		s.render(w, r, http.StatusOK, tokensReauthNotice(s.pageFor(r, "Tokens", "tokens")))
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	scope := formScope(r)
	expires, err := formExpires(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A blank name is a device pairing: auto-generate a friendly unique name,
	// retrying on a racing duplicate. A supplied name mints once and reports a
	// collision, unchanged.
	autogen := name == ""
	var secret string
	var row store.APIToken
	for attempt := 0; ; attempt++ {
		want := name
		if autogen {
			if want, err = s.autoDeviceName(r.Context()); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		secret, row, err = MintToken(r.Context(), s.store, want, scope, expires)
		if errors.Is(err, store.ErrDuplicate) {
			if autogen && attempt < 4 {
				continue
			}
			http.Error(w, "a token named "+want+" already exists; rotate it instead, or pick another name", http.StatusConflict)
			return
		}
		break
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: token new", "token", row.Name, "scope", row.Scope, "by", ident.token.Name)
	s.renderTokenResult(w, r, row, secret, false)
}

func (s *Server) handleTokenRotate(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if name == ident.token.Name {
		http.Error(w, "refusing to rotate the token this session is bound to; use another operator token", http.StatusConflict)
		return
	}
	if err := s.reauth(r); err != nil {
		s.render(w, r, http.StatusOK, tokensReauthNotice(s.pageFor(r, "Tokens", "tokens")))
		return
	}
	expires, err := formExpires(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	secret, row, err := RotateToken(r.Context(), s.store, name, expires, r.Form.Has("expires"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no token named "+name, http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: token rotate", "token", row.Name, "by", ident.token.Name)
	s.renderTokenResult(w, r, row, secret, true)
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if !r.Form.Has("confirmed") {
		http.Error(w, "delete requires confirmation", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	if name == ident.token.Name {
		http.Error(w, "refusing to revoke the token this session is bound to; use another operator token", http.StatusConflict)
		return
	}
	if err := s.store.RevokeToken(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no active token named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: token revoke", "token", name, "by", ident.token.Name)
	redirectFlash(w, r, "/tokens", "revoked "+name)
}

func (s *Server) renderTokenResult(w http.ResponseWriter, r *http.Request, row store.APIToken, secret string, rotated bool) {
	base := s.publicBaseURL(r)
	_, qr, err := s.pairingSVG(base, secret, row.Scope, row.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm := tokenResultVM{
		Page: s.pageFor(r, "Tokens", "tokens"), Rotated: rotated,
		Name: row.Name, Scope: row.Scope, Secret: secret, Finger: s.fingerprint,
		Expires: stampOrDashWeb(row.ExpiresAt),
		BaseURL: base, QRSVG: qr,
	}
	s.render(w, r, http.StatusOK, tokenResult(vm))
}

// formScope reads and defaults the scope field, refusing an unknown value.
func formScope(r *http.Request) string {
	switch sc := r.PostFormValue("scope"); sc {
	case store.ScopeOperator, store.ScopeReadonly, store.ScopeApprove:
		return sc
	default:
		return store.ScopeOperator
	}
}

// formExpires parses an optional Go duration; empty means never.
func formExpires(r *http.Request) (time.Duration, error) {
	v := r.PostFormValue("expires")
	if v == "" {
		return 0, nil
	}
	return time.ParseDuration(v)
}

func stampOrDashWeb(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// stateClass maps a lifecycle state to a chip variant.
func tokenStateClass(state string) string {
	switch state {
	case "active":
		return "ok"
	case "expired":
		return "wait"
	default:
		return "dim"
	}
}

func tokenStateWeb(t store.APIToken, now time.Time) string {
	switch {
	case t.RevokedAt != nil:
		return "revoked"
	case t.ExpiresAt != nil && !now.Before(*t.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}
