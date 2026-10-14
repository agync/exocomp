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
// shown exactly once, inline — never redirected, flashed, or logged, and never
// re-rendered by a form (invariant 4). Refusals re-render the tokens surface at
// 422 with the dialog carrying its field errors; flash is success-only.

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
	// Form is the mint dialog's refusal state (the netgroup pattern): nil at
	// rest, the filled form with field errors on a 422 re-render. Name, Scope,
	// and Expires are kept; confirm_token never re-renders.
	Form *tokenNewFormVM
	// Rotate is the rotate dialog's refusal state, one per refused row: nil at
	// rest, else the refused form the 422 re-render re-opens.
	Rotate *tokenRotateFormVM
	// RefusalMsg is the page-level refusal for a button-verb write with no
	// dialog of its own (a self rotate/revoke, a missing revoke confirmation),
	// rendered as a status line above the tray; RefusalName names the row the
	// retry re-submits.
	RefusalMsg  string
	RefusalName string
}

// tokenNewFormVM is the mint dialog's refusal state. Name, Scope, and Expires
// are the non-secret typed values a 422 keeps; confirm_token is never carried
// back. The minted secret itself never re-renders — it is shown once by
// tokenResult, never by this form (invariant 4).
type tokenNewFormVM struct {
	Name         string
	Scope        string
	Expires      string
	ErrName      string
	ErrExpires   string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *tokenNewFormVM) effective() tokenNewFormVM {
	if f == nil {
		return tokenNewFormVM{}
	}
	return *f
}

func (f *tokenNewFormVM) Pending() bool {
	return f != nil && f.Submitting
}

func (f *tokenNewFormVM) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

func (f *tokenNewFormVM) nameField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "tk-name", Label: "Token name", Name: "name", Value: v.Name,
		Placeholder: "blank to auto-generate",
		Help:        "Letters, digits, dots, hyphens, underscores. Blank mints a device-pairing token.",
		ErrMsg:      v.ErrName, FirstInvalid: v.FirstInvalid == "name",
	}
}

func (f *tokenNewFormVM) expiresField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "tk-exp", Label: "Expires (optional)", Name: "expires", Value: v.Expires,
		Placeholder: "never", Help: "A duration — e.g. 720h, 30d.",
		ErrMsg: v.ErrExpires, FirstInvalid: v.FirstInvalid == "expires",
	}
}

// tokenRotateFormVM is the rotate dialog's refusal state, one per refused row.
// Name is the target token so the 422 re-render re-opens the right dialog;
// Expires is the only non-secret typed value kept.
type tokenRotateFormVM struct {
	Name         string
	Expires      string
	ErrExpires   string
	ErrForm      string
	FirstInvalid string
	Submitting   bool
}

func (f *tokenRotateFormVM) effective() tokenRotateFormVM {
	if f == nil {
		return tokenRotateFormVM{}
	}
	return *f
}

func (vm tokensVM) rotateForm(name string) tokenRotateFormVM {
	if vm.Rotate != nil && vm.Rotate.Name == name {
		return *vm.Rotate
	}
	return tokenRotateFormVM{Name: name}
}

func (vm tokensVM) rotatePending() bool {
	return vm.Rotate != nil && vm.Rotate.Submitting
}

func (vm tokensVM) rotateErrForm() string {
	if vm.Rotate == nil {
		return ""
	}
	return vm.Rotate.ErrForm
}

func (vm tokensVM) rotateExpiresField(name string) fieldMeta {
	f := vm.rotateForm(name)
	return fieldMeta{
		ID: "tk-rot-exp-" + name, Label: "New expiry (optional)", Name: "expires", Value: f.Expires,
		Placeholder: "keep expiry", Help: "A duration — e.g. 720h, 30d.",
		ErrMsg: f.ErrExpires, FirstInvalid: f.FirstInvalid == "expires",
	}
}

type tokenRow struct {
	Name, Scope, Prefix, Expires, LastUsed, State string
	ExpiresAt                                     *time.Time
	LastUsedAt                                    *time.Time
	Self                                          bool
}

func (t tokenRow) ExpiresAtISO() string {
	if t.ExpiresAt != nil {
		return t.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return ""
}

func (t tokenRow) LastUsedAtISO() string {
	if t.LastUsedAt != nil {
		return t.LastUsedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return ""
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
	vm, err := s.tokensVMFor(r, nil, nil, "", "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, tokensPage(vm))
}

// The mint/rotate/revoke write surface's human refusal copy (forms/copy.txt):
// the raw validator words go to the log; the meta lines and status line carry
// these sentences.
const (
	tokenNameRefusal          = "Use letters, digits, dots, hyphens, underscores."
	tokenExpiryRefusal        = "That's not a duration — like 720h or 30d."
	tokenMintRefusal          = "Couldn't mint the token — nothing was saved."
	tokenRotateSubmitRefusal  = "Couldn't rotate the token — nothing was saved."
	tokenRevokeConfirmRefusal = "Confirm the revoke to take it down."
)

func tokenMissingRefusal(name string) string {
	return "No token called " + name + "."
}

func tokenSelfRefusal() string {
	return "That's the token this session is bound to — use another operator token."
}

func tokenNameRefusalFor(name string) string {
	if strings.Contains(name, " ") {
		return "Names can't have spaces."
	}
	return tokenNameRefusal
}

// tokensVMFor builds the tokens surface with the write dialogs' refusal state;
// nil form/rotate and an empty revoke message is the at-rest GET.
func (s *Server) tokensVMFor(r *http.Request, form *tokenNewFormVM, rotate *tokenRotateFormVM, refusalMsg, refusalName string) (tokensVM, error) {
	ident, _ := identityOf(r.Context())
	vm := tokensVM{
		Page: s.pageFor(r, "Tokens", "tokens"), Operator: canWrite(ident),
		Form: form, Rotate: rotate, RefusalMsg: refusalMsg, RefusalName: refusalName,
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "tokens-mint", Label: "token"}
		rows, err := s.store.ListTokens(r.Context(), false)
		if err != nil {
			return tokensVM{}, err
		}
		now := s.now()
		for _, t := range rows {
			vm.Tokens = append(vm.Tokens, tokenRow{
				Name: t.Name, Scope: t.Scope, Prefix: t.Prefix,
				Expires: stampOrDashWeb(t.ExpiresAt), LastUsed: stampOrDashWeb(t.LastUsedAt),
				ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt,
				State: tokenStateWeb(t, now), Self: t.Name == ident.token.Name,
			})
		}
	}
	return vm, nil
}

// tokenRefusal re-renders the tokens surface at 422 with the mint or rotate
// dialog carrying its refusals — the JS-off path, and the fragment body a JS
// submit swaps in place. Refusals never flash: flash is success-only (write.go).
func (s *Server) tokenRefusal(w http.ResponseWriter, r *http.Request, form *tokenNewFormVM, rotate *tokenRotateFormVM) {
	vm, err := s.tokensVMFor(r, form, rotate, "", "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, tokensPage(vm))
}

// tokenPageRefusal re-renders the tokens surface at 422 with a page-level
// status line naming the refused row (a button-verb write — self rotate or a
// revoke refusal — has no dialog of its own to carry field errors).
func (s *Server) tokenPageRefusal(w http.ResponseWriter, r *http.Request, name, msg string) {
	vm, err := s.tokensVMFor(r, nil, nil, msg, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, tokensPage(vm))
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
	expires := r.PostFormValue("expires")
	form := tokenNewFormVM{Name: name, Scope: scope, Expires: expires}

	// A blank name is a device pairing and needs no shape check; a supplied
	// name is held to the same rule the store's other names carry.
	if name != "" && !store.ValidName(name) {
		s.log.Info("web: token new refused", "err", "name shape", "by", ident.token.Name)
		form.ErrName = tokenNameRefusalFor(name)
		form.FirstInvalid = "name"
	}
	var expiry time.Duration
	if !validDuration(expires) {
		s.log.Info("web: token new refused", "err", "bad expiry", "by", ident.token.Name)
		form.ErrExpires = tokenExpiryRefusal
		if form.FirstInvalid == "" {
			form.FirstInvalid = "expires"
		}
	} else if expires != "" {
		expiry, _ = time.ParseDuration(expires)
	}
	if form.ErrName != "" || form.ErrExpires != "" {
		s.tokenRefusal(w, r, &form, nil)
		return
	}

	// Auto-generate a friendly unique name on a blank one, retrying on a racing
	// duplicate. A supplied name mints once and reports a collision, unchanged.
	autogen := name == ""
	var secret string
	var row store.APIToken
	var err error
	for attempt := 0; ; attempt++ {
		want := name
		if autogen {
			if want, err = s.autoDeviceName(r.Context()); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		secret, row, err = MintToken(r.Context(), s.store, want, scope, "", expiry)
		if errors.Is(err, store.ErrDuplicate) {
			if autogen && attempt < 4 {
				continue
			}
			s.log.Info("web: token new refused", "err", err.Error(), "by", ident.token.Name)
			form.ErrName = "A token called " + want + " already exists."
			form.FirstInvalid = "name"
			s.tokenRefusal(w, r, &form, nil)
			return
		}
		break
	}
	if err != nil {
		s.log.Error("web: token new", "err", err.Error())
		form.ErrForm = tokenMintRefusal
		s.tokenRefusal(w, r, &form, nil)
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
		// The self row never renders a rotate dialog (the page shows "this
		// session"), so this direct-POST refusal lands on the page-level
		// status line, not a dialog.
		s.log.Info("web: token rotate refused", "err", "self", "by", ident.token.Name)
		s.tokenPageRefusal(w, r, name, tokenSelfRefusal())
		return
	}
	if err := s.reauth(r); err != nil {
		s.render(w, r, http.StatusOK, tokensReauthNotice(s.pageFor(r, "Tokens", "tokens")))
		return
	}
	expires := r.PostFormValue("expires")
	form := tokenRotateFormVM{Name: name, Expires: expires}
	var expiry time.Duration
	if !validDuration(expires) {
		s.log.Info("web: token rotate refused", "err", "bad expiry", "by", ident.token.Name)
		form.ErrExpires = tokenExpiryRefusal
		form.FirstInvalid = "expires"
	}
	if form.ErrExpires != "" {
		s.tokenRefusal(w, r, nil, &form)
		return
	}
	if expires != "" {
		expiry, _ = time.ParseDuration(expires)
	}
	secret, row, err := RotateToken(r.Context(), s.store, name, expiry, r.Form.Has("expires"))
	if errors.Is(err, store.ErrNotFound) {
		s.log.Info("web: token rotate refused", "err", "no such token", "by", ident.token.Name)
		form.ErrForm = tokenMissingRefusal(name)
		s.tokenRefusal(w, r, nil, &form)
		return
	}
	if err != nil {
		s.log.Error("web: token rotate", "err", err.Error())
		form.ErrForm = tokenRotateSubmitRefusal
		s.tokenRefusal(w, r, nil, &form)
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
	name := r.PathValue("name")
	if !r.Form.Has("confirmed") {
		s.log.Info("web: token revoke refused", "err", "no confirmation", "by", ident.token.Name)
		s.tokenPageRefusal(w, r, name, tokenRevokeConfirmRefusal)
		return
	}
	if name == ident.token.Name {
		s.log.Info("web: token revoke refused", "err", "self", "by", ident.token.Name)
		s.tokenPageRefusal(w, r, name, tokenSelfRefusal())
		return
	}
	if err := s.store.RevokeToken(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.log.Info("web: token revoke refused", "err", "no such token", "by", ident.token.Name)
			s.tokenPageRefusal(w, r, name, tokenMissingRefusal(name))
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: token revoke", "token", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/tokens", "revoked "+name)
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
