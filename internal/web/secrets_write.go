package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// Secrets add/rotate seal a plaintext into the envelope, so they need the Sealer
// (nil → 503) and are gated behind reauth. The submitted value is a password
// field — never echoed, flashed, redirected, or logged (invariant 4). Delete is
// guarded by the run-secret bindings that reference the secret.

func init() { registerWrite((*Server).registerSecretWrite) }

func (s *Server) registerSecretWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /secrets/add", s.requireAuth(s.handleSecretAdd))
	mux.HandleFunc("POST /secrets/{name}/rotate", s.requireAuth(s.handleSecretRotate))
	mux.HandleFunc("POST /secrets/{name}/rm", s.requireAuth(s.handleSecretRm))
}

func (s *Server) handleSecretAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if s.sealer == nil {
		http.Error(w, "sealing is not available on this surface", http.StatusServiceUnavailable)
		return
	}
	if err := s.reauth(r); err != nil {
		s.render(w, r, http.StatusOK, secretReauthNotice(s.pageFor(r, "Secrets", "secrets")))
		return
	}
	name := r.PostFormValue("name")
	kind := formKind(r)
	value, err := formSecretValue(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sealed, err := s.sealer.Seal(name, kind, value)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.store.CreateSecret(r.Context(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		AAD: sealed.Name + "|" + sealed.Kind, Source: r.PostFormValue("source"), ExpiresAt: formExpiresAt(r),
	}); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			http.Error(w, "a secret named "+name+" already exists; rotate it instead", http.StatusConflict)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: secret add", "secret", name, "kind", kind, "by", ident.token.Name)
	redirectFlash(w, r, "/secrets", "sealed "+name)
}

func (s *Server) handleSecretRotate(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if s.sealer == nil {
		http.Error(w, "sealing is not available on this surface", http.StatusServiceUnavailable)
		return
	}
	if err := s.reauth(r); err != nil {
		s.render(w, r, http.StatusOK, secretReauthNotice(s.pageFor(r, "Secrets", "secrets")))
		return
	}
	name := r.PathValue("name")
	existing, err := s.store.GetSecret(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no secret named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	value, err := formSecretValue(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sealed, err := s.sealer.Seal(existing.Name, existing.Kind, value)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.RotateSecret(r.Context(), name, sealed.Nonce, sealed.Ciphertext); err != nil {
		s.fail(w, r, err)
		return
	}
	if r.Form.Has("expires") {
		if err := s.store.SetSecretExpiry(r.Context(), name, formExpiresAt(r)); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.log.Info("web: secret rotate", "secret", name, "by", ident.token.Name)
	redirectFlash(w, r, "/secrets", "rotated "+name)
}

func (s *Server) handleSecretRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if !r.Form.Has("confirmed") {
		http.Error(w, "delete requires confirmation", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	refs, err := s.secretBindings(r, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(refs) > 0 && !r.Form.Has("force") {
		http.Error(w, "refusing to delete "+name+": still bound by run-secret(s); pass force to override", http.StatusConflict)
		return
	}
	if err := s.store.DeleteSecret(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no secret named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: secret rm", "secret", name, "forced_bindings", len(refs), "by", ident.token.Name)
	redirectFlash(w, r, "/secrets", "deleted "+name)
}

// secretBindings returns the run-secret bindings that reference a secret, the
// guard the delete path checks.
func (s *Server) secretBindings(r *http.Request, name string) ([]store.RunSecret, error) {
	sets, err := s.store.ListRunSecretBindings(r.Context())
	if err != nil {
		return nil, err
	}
	var refs []store.RunSecret
	for _, set := range sets {
		pairs, err := s.store.RunSecretsByBinding(r.Context(), set)
		if err != nil {
			return nil, err
		}
		for _, p := range pairs {
			if p.SecretName == name {
				refs = append(refs, p)
			}
		}
	}
	return refs, nil
}

func formKind(r *http.Request) string {
	if k := r.PostFormValue("kind"); k != "" {
		return k
	}
	return "generic"
}

// formSecretValue returns the plaintext to seal: a generated random value when
// the generate box is set, else the submitted value. Never logged.
func formSecretValue(r *http.Request) ([]byte, error) {
	if r.Form.Has("generate") {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		enc := base64.RawURLEncoding.EncodeToString(b)
		return []byte(enc), nil
	}
	v := r.PostFormValue("value")
	if v == "" {
		return nil, errors.New("a value is required (or check generate)")
	}
	return []byte(v), nil
}

// formExpiresAt turns an optional duration field into an absolute instant.
func formExpiresAt(r *http.Request) *time.Time {
	v := r.PostFormValue("expires")
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return nil
	}
	at := time.Now().UTC().Add(d)
	return &at
}
