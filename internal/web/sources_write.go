package web

import (
	"errors"
	"net/http"
	"path/filepath"

	"exocomp.dev/exocomp/internal/naming"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
)

// A path source is a plain registration: no secret material, so
// add-path/enable/disable/rm are operator-gated only, like a source's other
// non-sensitive edits. A git source's add generates and seals a deploy key
// (mirrors cmd/exocomp/source.go's `source add --url`), so it needs the Sealer
// (nil -> 503) and reauth, the same shape as secrets add. The generated public
// key is shown exactly once, inline, never redirected or logged.
//
// `source sync` (clone/fetch + pin advance) is deliberately NOT exposed here:
// there is no queued job kind for it yet, and running a git fetch inline on an
// HTTP handler would need its own driver (working tree, timeout, and progress
// story) that the CLI path gets from being a foreground command. Until that
// driver exists, sync stays a CLI-only verb (`exocomp source sync`).

func init() { registerWrite((*Server).registerSourceWrite) }

// deployKeyKind and sourceDeployKeySecretName mirror cmd/exocomp/source.go's
// deployKeyKind/deployKeySecretName so a web-registered git source's deploy key
// lands under the same sealed-secret naming convention as a CLI-registered one.
const deployKeyKind = "deploy-key"

func sourceDeployKeySecretName(name string) string { return "source/" + name + "/deploy-key" }

func (s *Server) registerSourceWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /sources/add-path", s.requireAuth(s.handleSourceAddPath))
	mux.HandleFunc("POST /sources/add-git", s.requireAuth(s.handleSourceAddGit))
	mux.HandleFunc("POST /sources/{name}/enable", s.requireAuth(s.handleSourceSetEnabled(true)))
	mux.HandleFunc("POST /sources/{name}/disable", s.requireAuth(s.handleSourceSetEnabled(false)))
	mux.HandleFunc("POST /sources/{name}/rm", s.requireAuth(s.handleSourceRm))
}

// sourceGitResultVM shows a freshly generated deploy key's public half once —
// the only place a git source's key material appears on this surface.
type sourceGitResultVM struct {
	Page                page
	Name, URL, Ref      string
	PublicAuthorizedKey string
}

func (s *Server) handleSourceAddPath(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PostFormValue("name")
	if err := naming.ValidateComponent("source", name); err != nil {
		s.fail(w, r, err)
		return
	}
	rawPath := r.PostFormValue("path")
	if rawPath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	// An absolute path is cleaned as-is; a relative path is kept verbatim
	// (cleaned) so it resolves against the data volume at run time, mirroring the
	// CLI. Read/write is the writable checkbox, defaulting off (read-only).
	uri := filepath.Clean(rawPath)
	writable := r.PostFormValue("writable") == "on"
	if _, err := s.store.CreateSource(r.Context(), store.Source{
		Name: name, Kind: store.SourcePath, URI: uri, Writable: writable, Enabled: true,
	}); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			http.Error(w, "a source named "+name+" already exists", http.StatusConflict)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: source add-path", "source", name, "writable", writable, "by", ident.token.Name)
	redirectFlash(w, r, "/sources", "registered "+name)
}

func (s *Server) handleSourceAddGit(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if s.sealer == nil {
		http.Error(w, "sealing is not available on this surface", http.StatusServiceUnavailable)
		return
	}
	if err := s.reauth(r); err != nil {
		s.render(w, r, http.StatusOK, sourceReauthNotice(s.pageFor(r, "Sources", "sources")))
		return
	}
	name := r.PostFormValue("name")
	if err := naming.ValidateComponent("source", name); err != nil {
		s.fail(w, r, err)
		return
	}
	url := r.PostFormValue("url")
	if url == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	ref := r.PostFormValue("ref")
	if ref == "" {
		ref = "main"
	}

	key, err := source.GenerateDeployKey(name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	secretName := sourceDeployKeySecretName(name)

	// Create the source row first, mirroring runSourceAdd's ordering: a
	// duplicate name is refused before any key material is sealed or stored,
	// so a colliding name never leaves a sealed deploy key with nothing
	// referencing it. A later failure rolls the row back best-effort.
	if _, err := s.store.CreateSource(r.Context(), store.Source{
		Name: name, Kind: store.SourceGit, URI: url, Ref: ref,
		DeployKeySecret: secretName, Enabled: true,
	}); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			http.Error(w, "a source named "+name+" already exists", http.StatusConflict)
			return
		}
		s.fail(w, r, err)
		return
	}

	sealed, err := s.sealer.Seal(secretName, deployKeyKind, key.PrivatePEM)
	if err != nil {
		if derr := s.store.DeleteSource(r.Context(), name); derr != nil {
			s.log.Error("web: source add-git rollback failed", "source", name, "seal_err", err, "rollback_err", derr)
		}
		s.fail(w, r, err)
		return
	}
	if _, err := s.store.CreateSecret(r.Context(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		if derr := s.store.DeleteSource(r.Context(), name); derr != nil {
			s.log.Error("web: source add-git rollback failed", "source", name, "store_err", err, "rollback_err", derr)
		}
		s.fail(w, r, err)
		return
	}

	s.log.Info("web: source add-git", "source", name, "by", ident.token.Name)
	vm := sourceGitResultVM{
		Page: s.pageFor(r, "Sources", "sources"), Name: name, URL: url, Ref: ref,
		PublicAuthorizedKey: key.PublicAuthorizedKey,
	}
	s.render(w, r, http.StatusOK, sourceGitResult(vm))
}

// handleSourceSetEnabled returns a handler that flips a source's enabled bit,
// factored so /enable and /disable share one implementation (mirrors the
// enable/disable pair a schedule or runner toggle would take).
func (s *Server) handleSourceSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := s.writeGuard(w, r)
		if !ok {
			return
		}
		name := r.PathValue("name")
		src, err := s.store.SourceByName(r.Context(), name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "no source named "+name, http.StatusNotFound)
				return
			}
			s.fail(w, r, err)
			return
		}
		if err := s.store.SetSourceEnabled(r.Context(), src.ID, enabled); err != nil {
			s.fail(w, r, err)
			return
		}
		verb := "disabled"
		if enabled {
			verb = "enabled"
		}
		s.log.Info("web: source "+verb, "source", name, "by", ident.token.Name)
		redirectFlash(w, r, "/sources", verb+" "+name)
	}
}

func (s *Server) handleSourceRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if !r.Form.Has("confirmed") {
		http.Error(w, "delete requires confirmation", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeleteSource(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no source named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: source rm", "source", name, "by", ident.token.Name)
	redirectFlash(w, r, "/sources", "removed "+name)
}
