package web

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"exocomp.dev/exocomp/internal/deps"
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
	mux.HandleFunc("POST /sources/add", s.requireAuth(s.handleSourceAdd))
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

// handleSourceAdd is the single "Add a source" entry point: it reads the one
// "Path or git URL" field the form submits and routes on
// source.LooksLikeGitURL, so the operator never has to pick a kind up front.
// The guard, reauth, and sealer checks the git branch needs stay inside
// addGitSource — a path add never touches them.
func (s *Server) handleSourceAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PostFormValue("name")
	if err := naming.ValidateComponent("source", name); err != nil {
		s.fail(w, r, err)
		return
	}
	location := strings.TrimSpace(r.PostFormValue("location"))
	if location == "" {
		http.Error(w, "path or git URL is required", http.StatusBadRequest)
		return
	}
	if source.LooksLikeGitURL(location) {
		s.addGitSource(w, r, ident, name, location)
		return
	}
	s.addPathSource(w, r, ident, name, location)
}

// addPathSource registers a plain path source: no secret material, so it is
// operator-gated only (handleSourceAdd's writeGuard), like the source's other
// non-sensitive edits.
func (s *Server) addPathSource(w http.ResponseWriter, r *http.Request, ident identity, name, rawPath string) {
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

// addGitSource registers a git source: it generates and seals a deploy key
// (mirrors cmd/exocomp/source.go's `source add --url`), so it needs the
// Sealer (nil -> 503) and reauth, the same shape as secrets add. The generated
// public key is shown exactly once, inline, never redirected or logged.
func (s *Server) addGitSource(w http.ResponseWriter, r *http.Request, ident identity, name, url string) {
	if s.sealer == nil {
		http.Error(w, "sealing is not available on this surface", http.StatusServiceUnavailable)
		return
	}
	if err := s.reauth(r); err != nil {
		s.render(w, r, http.StatusOK, sourceReauthNotice(s.pageFor(r, "Sources", "sources")))
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
	// The rm needs the row to resolve the caches the source may own, and it is
	// gone after DeleteSource — pre-fetch.
	src, err := s.store.SourceByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no source named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	if err := s.store.DeleteSource(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no source named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.purgeSourceCaches(r.Context(), src)
	s.log.Info("web: source rm", "source", name, "by", ident.token.Name)
	redirectFlash(w, r, "/sources", "removed "+name)
}

// purgeSourceCaches removes a deleted source's dependency caches
// (cache/python/<SourceKey> and cache/collections/<SourceKey> under the data
// dir) unless another registered source resolves to the same root and therefore
// shares the key (ADR 0015). A disabled source still owns its cache when
// re-enabled, so it counts as a reference. The source row is already deleted by
// the caller. Any failure leaves the caches in place: the failure mode is a
// stale cache, never a wrong purge.
func (s *Server) purgeSourceCaches(ctx context.Context, src store.Source) {
	loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return
	}
	key := deps.SourceKey(loc.Root)
	rows, err := s.store.ListSources(ctx)
	if err != nil {
		return
	}
	for _, other := range rows {
		otherLoc, err := source.Resolve(s.dataDir, other.Kind, other.Name, other.URI, other.Writable)
		if err != nil {
			continue
		}
		if deps.SourceKey(otherLoc.Root) == key {
			return
		}
	}
	if err := deps.RemoveProjectCaches(s.dataDir, loc.Root); err != nil {
		s.log.Warn("web: source rm: purge caches failed", "source", src.Name, "error", err)
	}
}
