package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// Secrets add/rotate seal a plaintext into the envelope, so they need the Sealer
// (nil → 503) and are gated behind reauth. The submitted value is a password
// field — never echoed, flashed, redirected, or logged (invariant 4): a 422
// re-render keeps the non-secret fields only, and the value field always
// renders empty. Delete is guarded by the run-secret bindings that reference
// the secret (its refusal handling rides the shared row-actions region).

func init() { registerWrite((*Server).registerSecretWrite) }

func (s *Server) registerSecretWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /secrets/add", s.requireAuth(s.handleSecretAdd))
	mux.HandleFunc("POST /secrets/{name}/rotate", s.requireAuth(s.handleSecretRotate))
	mux.HandleFunc("POST /secrets/{name}/rm", s.requireAuth(s.handleSecretRm))
	mux.HandleFunc("POST /secrets/{rest...}", s.requireAuth(s.handleSecretPathAction))
}

// secretNamePattern bounds a secret's name — the shape the form's pattern
// attribute used to carry, now held server-side (the daemon is authoritative).
var secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func validSecretName(s string) bool { return secretNamePattern.MatchString(s) }

// validDuration reports whether s parses as a positive duration; empty means
// "no expiry" and is not a refusal.
func validDuration(s string) bool {
	if s == "" {
		return true
	}
	d, err := time.ParseDuration(s)
	return err == nil && d > 0
}

// The seal-a-secret and change (replace/rotate) dialogs' human refusal copy
// (forms/copy.txt): the raw validator words go to the log; the meta lines and
// status line carry these sentences.
const (
	secretNameRefusal   = "Use letters, digits, dots, hyphens, underscores, slashes."
	secretValueRefusal  = "Give the secret a value, or generate one."
	secretExpiryRefusal = "That's not a duration — like 720h or 30d."
	secretSubmitRefusal = "Couldn't seal the secret — nothing was saved."
	// secretRemoveConfirmRefusal is the rm verb's confirm-gate refusal
	// (forms/SPEC.md): the shared confirm dialog's hidden confirmed=1 is the
	// safety, and a confirmed-less remove is refused on the row.
	secretRemoveConfirmRefusal = "Nothing was removed — confirm the removal in the dialog."
)

// secretChangeSubmitRefusal names the whole-submit refusal for the source's
// honest verb: a generated secret is rotated, an operator-supplied one is only
// replaced.
func secretChangeSubmitRefusal(src string) string {
	if src == store.SecretSourceGenerated {
		return "Couldn't rotate the secret — nothing was saved."
	}
	return "Couldn't replace the secret — nothing was saved."
}

// secretChangePastVerb is the success flash's past-tense verb for a source.
func secretChangePastVerb(src string) string {
	if src == store.SecretSourceGenerated {
		return "rotated"
	}
	return "replaced"
}

// secretBoundRefusal is the rm verb's binding refusal (forms/SPEC.md): a bound
// secret stays, and the row says so — the CLI's --force override stays on the
// CLI, so the row's copy names the binding without offering it.
func secretBoundRefusal(name string) string {
	return "Nothing was removed — " + name + " is still bound as a run secret."
}

func secretMissingRefusal(name string) string {
	return "No secret called " + name + "."
}

func secretNameSpaced() string {
	return "Names can't have spaces."
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
	expires := r.PostFormValue("expires")
	form := secretAddFormVM{Name: name, Kind: kind, Expires: expires}

	// Validate up front so every refusal renders at once (the daemon stays
	// authoritative: these rules only shape the response).
	if !validSecretName(name) {
		s.log.Info("web: secret add refused", "err", "name shape", "by", ident.token.Name)
		if strings.Contains(name, " ") {
			form.ErrName = secretNameSpaced()
		} else {
			form.ErrName = secretNameRefusal
		}
	}
	if !r.Form.Has("generate") && r.PostFormValue("value") == "" {
		s.log.Info("web: secret add refused", "err", "empty value", "by", ident.token.Name)
		form.ErrValue = secretValueRefusal
	}
	if !validDuration(expires) {
		s.log.Info("web: secret add refused", "err", "bad expiry", "by", ident.token.Name)
		form.ErrExpires = secretExpiryRefusal
	}
	switch {
	case form.ErrName != "":
		form.FirstInvalid = "name"
	case form.ErrValue != "":
		form.FirstInvalid = "value"
	case form.ErrExpires != "":
		form.FirstInvalid = "expires"
	}
	if form.ErrName != "" || form.ErrValue != "" || form.ErrExpires != "" {
		s.secretRefusal(w, r, &form, nil)
		return
	}

	value, err := formSecretValue(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sealed, err := s.sealer.Seal(name, kind, value)
	if err != nil {
		s.log.Error("web: secret add", "err", err.Error())
		form.ErrForm = secretSubmitRefusal
		s.secretRefusal(w, r, &form, nil)
		return
	}
	if _, err := s.store.CreateSecret(r.Context(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		AAD: sealed.Name + "|" + sealed.Kind, Source: r.PostFormValue("source"), ExpiresAt: formExpiresAt(r),
	}); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			s.log.Info("web: secret add refused", "err", err.Error(), "by", ident.token.Name)
			form.ErrName = "A secret called " + name + " already exists."
			form.FirstInvalid = "name"
			s.secretRefusal(w, r, &form, nil)
			return
		}
		s.log.Error("web: secret add", "err", err.Error())
		form.ErrForm = secretSubmitRefusal
		s.secretRefusal(w, r, &form, nil)
		return
	}
	s.log.Info("web: secret add", "secret", name, "kind", kind, "by", ident.token.Name)
	s.redirectFlash(w, r, "/secrets", "sealed "+name)
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
			s.log.Info("web: secret rotate refused", "err", "no such secret", "by", ident.token.Name)
			form := secretRotateFormVM{Name: name, ErrForm: secretMissingRefusal(name)}
			s.secretRefusal(w, r, nil, &form)
			return
		}
		s.fail(w, r, err)
		return
	}
	expires := r.PostFormValue("expires")
	form := secretRotateFormVM{Name: name, Expires: expires}

	if !r.Form.Has("generate") && r.PostFormValue("value") == "" {
		s.log.Info("web: secret rotate refused", "err", "empty value", "by", ident.token.Name)
		form.ErrValue = secretValueRefusal
		form.FirstInvalid = "value"
	}
	if !validDuration(expires) {
		s.log.Info("web: secret rotate refused", "err", "bad expiry", "by", ident.token.Name)
		form.ErrExpires = secretExpiryRefusal
		if form.FirstInvalid == "" {
			form.FirstInvalid = "expires"
		}
	}
	if form.ErrValue != "" || form.ErrExpires != "" {
		s.secretRefusal(w, r, nil, &form)
		return
	}
	value, err := formSecretValue(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sealed, err := s.sealer.Seal(existing.Name, existing.Kind, value)
	if err != nil {
		s.log.Error("web: secret rotate", "err", err.Error())
		form.ErrForm = secretChangeSubmitRefusal(existing.Source)
		s.secretRefusal(w, r, nil, &form)
		return
	}
	if err := s.store.RotateSecret(r.Context(), name, sealed.Nonce, sealed.Ciphertext); err != nil {
		s.log.Error("web: secret rotate", "err", err.Error())
		form.ErrForm = secretChangeSubmitRefusal(existing.Source)
		s.secretRefusal(w, r, nil, &form)
		return
	}
	if r.Form.Has("expires") {
		if err := s.store.SetSecretExpiry(r.Context(), name, formExpiresAt(r)); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	// A secret change (replace or rotate) alters the envelope the recovery
	// kit's derivation rule draws from, so the kit is stale until it is
	// reprinted (SPEC §7 A3).
	s.bumpKitChanges(r.Context())
	s.log.Info("web: secret rotate", "secret", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/secrets", secretChangePastVerb(existing.Source)+" "+name)
}

// secretRefusal re-renders the secrets surface at 422 with the add or rotate
// dialog carrying its refusals — the JS-off path, and the fragment body a JS
// submit swaps in place. Refusals never flash: flash is success-only (write.go).
func (s *Server) secretRefusal(w http.ResponseWriter, r *http.Request, form *secretAddFormVM, rotate *secretRotateFormVM) {
	vm, err := s.secretsVMFor(r, form, rotate, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, secretsList(vm))
}

// secretsVMFor builds the secrets surface with the write dialogs' refusal
// state and a refused row verb's state (nil at rest; the rm refusal renders
// on the refused row, forms/SPEC.md); the GET path (handlers.go) builds the
// same shape without either.
func (s *Server) secretsVMFor(r *http.Request, form *secretAddFormVM, rotate *secretRotateFormVM, refusal *rowRefusal) (secretsVM, error) {
	rows, err := s.secrets(r)
	if err != nil {
		return secretsVM{}, err
	}
	if refusal != nil {
		for i := range rows {
			if rows[i].Name == refusal.Name {
				rows[i].Refusal = refusal.Msg
			}
		}
	}
	ident, _ := identityOf(r.Context())
	vm := secretsVM{
		Page: s.pageFor(r, "Secrets", "secrets"), Secrets: rows,
		Operator: canWrite(ident), SealAvailable: s.sealer != nil,
		// A2: a refusal re-render keeps the ?key= deep-link focus, so a
		// refused rotate on the focused row re-renders the row focused.
		FocusKey: r.URL.Query().Get("key"),
		Form:     form, Rotate: rotate,
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "secrets-add", Label: "secret"}
	}
	return vm, nil
}

func (s *Server) handleSecretRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !r.Form.Has("confirmed") {
		s.secretRowRefusal(w, r, name, secretRemoveConfirmRefusal)
		return
	}
	refs, err := s.secretBindings(r, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(refs) > 0 && !r.Form.Has("force") {
		// The row has no force control (the CLI's --force stays CLI); the
		// bound secret is refused on the row, never a flash.
		s.secretRowRefusal(w, r, name, secretBoundRefusal(name))
		return
	}
	if err := s.store.DeleteSecret(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No secret named "+name+" exists to remove.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: secret rm", "secret", name, "forced_bindings", len(refs), "by", ident.token.Name)
	s.redirectFlash(w, r, "/secrets", "deleted "+name)
}

// secretRowRefusal re-renders the secrets surface at 422 with the rm verb's
// refusal on the refused row — the forms/SPEC.md button-verb shape: a refusal
// renders on the surface that holds the verb, never a flash (flash is
// success-only, write.go).
func (s *Server) secretRowRefusal(w http.ResponseWriter, r *http.Request, name, msg string) {
	vm, err := s.secretsVMFor(r, nil, nil, &rowRefusal{Name: name, Msg: msg})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, secretsList(vm))
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

// handleSecretPathAction dispatches /secrets/<name>/<verb> for secret names that
// contain slashes, which the single-segment ServeMux patterns above cannot
// capture. Names such as ntfy/url and goalert/url are normal.
func (s *Server) handleSecretPathAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.PathValue("rest"), "/")
	if rest == "" || rest == "add" {
		http.NotFound(w, r)
		return
	}
	for _, verb := range []string{"/rotate", "/rm"} {
		if strings.HasSuffix(rest, verb) {
			name := strings.TrimSuffix(rest, verb)
			name = strings.TrimSuffix(name, "/")
			if name == "" || !validSecretName(name) {
				http.NotFound(w, r)
				return
			}
			// Restore the two-path shape handlers already expect via PathValue.
			r.SetPathValue("name", name)
			if verb == "/rotate" {
				s.handleSecretRotate(w, r)
			} else {
				s.handleSecretRm(w, r)
			}
			return
		}
	}
	http.NotFound(w, r)
}
