package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"exocomp.dev/exocomp/internal/deps"
	"exocomp.dev/exocomp/internal/naming"
	"exocomp.dev/exocomp/internal/secrets"
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
	mux.HandleFunc("POST /sources/{name}/sync", s.requireAuth(s.handleSourceSync))
	mux.HandleFunc("POST /sources/{name}/enable", s.requireAuth(s.handleSourceSetEnabled(true)))
	mux.HandleFunc("POST /sources/{name}/disable", s.requireAuth(s.handleSourceSetEnabled(false)))
	mux.HandleFunc("POST /sources/{name}/egress", s.requireAuth(s.handleSourceEgress))
	mux.HandleFunc("POST /sources/{name}/run-secrets", s.requireAuth(s.handleSourceRunSecrets))
	mux.HandleFunc("POST /sources/{name}/rm", s.requireAuth(s.handleSourceRm))
}

// sourceGitResultVM shows a freshly generated deploy key's public half once —
// the only place a git source's key material appears on this surface.
type sourceGitResultVM struct {
	Page                page
	Name, URL, Ref      string
	PublicAuthorizedKey string
}

// The add-source dialog's human refusal copy (the netgroup pattern): the raw
// validator words go to the log; the form's meta lines and status line carry
// these sentences.
const (
	sourceNameRefusal     = "Use letters, digits, dots, hyphens, underscores."
	sourceLocationRefusal = "A path or git URL is required."
	sourceSubmitRefusal   = "Could not add the source — check the daemon log."
)

func sourceDuplicateRefusal(name string) string {
	return "A source named " + name + " already exists."
}

// handleSourceAdd is the single "Add a source" entry point: it reads the one
// "Path or git URL" field the form submits and routes on
// source.LooksLikeGitURL, so the operator never has to pick a kind up front.
// Field refusals re-render the dialog at 422 with the typed values kept (the
// netgroup pattern): the daemon stays authoritative, the client rules only
// shape the response. The guard, reauth, and sealer checks the git branch
// needs stay inside addGitSource — a path add never touches them.
func (s *Server) handleSourceAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PostFormValue("name")
	location := strings.TrimSpace(r.PostFormValue("location"))
	form := sourceFormVM{Name: name, Location: location, Ref: r.PostFormValue("ref")}

	if err := naming.ValidateComponent("source", name); err != nil {
		s.log.Info("web: source add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrName = sourceNameRefusal
		form.FirstInvalid = "name"
		s.sourceRefusal(w, r, form)
		return
	}
	if location == "" {
		s.log.Info("web: source add refused", "err", "empty location", "by", ident.token.Name)
		form.ErrLocation = sourceLocationRefusal
		form.FirstInvalid = "location"
		s.sourceRefusal(w, r, form)
		return
	}
	if source.LooksLikeGitURL(location) {
		s.addGitSource(w, r, ident, name, location, form)
		return
	}
	s.addPathSource(w, r, ident, name, location, form)
}

// sourceRefusal re-renders the sources surface at 422 with the add dialog
// carrying its refusals — the JS-off path, and the fragment body a JS submit
// swaps in place. Refusals never flash: flash is success-only (write.go).
func (s *Server) sourceRefusal(w http.ResponseWriter, r *http.Request, form sourceFormVM) {
	vm, err := s.sourcesVMFor(r, &form)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, sourcesList(vm))
}

// sourcesVMFor builds the sources surface with the add dialog's refusal state.
func (s *Server) sourcesVMFor(r *http.Request, form *sourceFormVM) (sourcesVM, error) {
	rows, err := s.sources(r)
	if err != nil {
		return sourcesVM{}, err
	}
	ident, _ := identityOf(r.Context())
	vm := sourcesVM{
		Page: s.pageFor(r, "Sources", "sources"), Sources: rows,
		Operator: canWrite(ident), SealAvailable: s.sealer != nil,
		Form: form,
	}
	// The app bar's page-own fact: the registered source count.
	vm.Page.Meta = sourcesMeta(len(rows))
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "sources-add", Label: "source"}
	}
	return vm, nil
}

// addPathSource registers a plain path source: no secret material, so it is
// operator-gated only (handleSourceAdd's writeGuard), like the source's other
// non-sensitive edits.
func (s *Server) addPathSource(w http.ResponseWriter, r *http.Request, ident identity, name, rawPath string, form sourceFormVM) {
	// An absolute path is cleaned as-is; a relative path is kept verbatim
	// (cleaned) so it resolves against the data volume at run time, mirroring the
	// CLI. Read/write is the writable checkbox, defaulting off (read-only).
	uri := filepath.Clean(rawPath)
	writable := r.PostFormValue("writable") == "on"
	if _, err := s.store.CreateSource(r.Context(), store.Source{
		Name: name, Kind: store.SourcePath, URI: uri, Writable: writable, Enabled: true,
	}); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			s.log.Info("web: source add refused", "err", err.Error(), "by", ident.token.Name)
			form.ErrName = sourceDuplicateRefusal(name)
			form.FirstInvalid = "name"
			s.sourceRefusal(w, r, form)
			return
		}
		s.log.Error("web: source add", "err", err.Error())
		form.ErrForm = sourceSubmitRefusal
		s.sourceRefusal(w, r, form)
		return
	}
	s.log.Info("web: source add-path", "source", name, "writable", writable, "by", ident.token.Name)
	s.redirectFlash(w, r, "/sources", "registered "+name)
}

// addGitSource registers a git source: it generates and seals a deploy key
// (mirrors cmd/exocomp/source.go's `source add --url`), so it needs the
// Sealer (nil -> 503) and reauth, the same shape as secrets add. The generated
// public key is shown exactly once, inline, never redirected or logged.
func (s *Server) addGitSource(w http.ResponseWriter, r *http.Request, ident identity, name, url string, form sourceFormVM) {
	if s.sealer == nil {
		s.log.Info("web: source add refused", "err", "no sealer", "by", ident.token.Name)
		form.ErrForm = "Sealing is not available on this surface (this process holds no envelope)."
		s.sourceRefusal(w, r, form)
		return
	}
	if err := s.reauth(r); err != nil {
		s.render(w, r, http.StatusOK, sourceReauthNotice(s.pageFor(r, "Sources", "sources")))
		return
	}
	ref := form.Ref
	if ref == "" {
		ref = "main"
	}

	publicKey, err := s.createGitSource(r.Context(), store.Source{Name: name, URI: url, Ref: ref})
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			s.log.Info("web: source add refused", "err", err.Error(), "by", ident.token.Name)
			form.ErrName = sourceDuplicateRefusal(name)
			form.FirstInvalid = "name"
			s.sourceRefusal(w, r, form)
			return
		}
		s.log.Error("web: source add-git", "err", err.Error())
		form.ErrForm = sourceSubmitRefusal
		s.sourceRefusal(w, r, form)
		return
	}

	s.log.Info("web: source add-git", "source", name, "by", ident.token.Name)
	vm := sourceGitResultVM{
		Page: s.pageFor(r, "Sources", "sources"), Name: name, URL: url, Ref: ref,
		PublicAuthorizedKey: publicKey,
	}
	s.render(w, r, http.StatusOK, sourceGitResult(vm))
}

// createGitSource records a git source and seals the deploy key it generates
// (mirrors cmd/exocomp/source.go's `source add --url`), returning the public
// half to add to the forge. The row is created first, so a duplicate name is
// store.ErrDuplicate before any key material is sealed or stored, and a later
// failure rolls the row back best-effort. The caller has checked s.sealer.
func (s *Server) createGitSource(ctx context.Context, row store.Source) (string, error) {
	key, err := source.GenerateDeployKey(row.Name)
	if err != nil {
		return "", err
	}
	row.Kind = store.SourceGit
	row.DeployKeySecret = sourceDeployKeySecretName(row.Name)
	row.Enabled = true
	if _, err := s.store.CreateSource(ctx, row); err != nil {
		return "", err
	}
	rollback := func(cause error) error {
		if derr := s.store.DeleteSource(ctx, row.Name); derr != nil {
			s.log.Error("web: source add-git rollback failed", "source", row.Name, "err", cause, "rollback_err", derr)
		}
		return cause
	}
	sealed, err := s.sealer.Seal(row.DeployKeySecret, deployKeyKind, key.PrivatePEM)
	if err != nil {
		return "", rollback(err)
	}
	if _, err := s.store.CreateSecret(ctx, store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			// A key left sealed by an earlier source of this name, not a
			// duplicate source: say so rather than blame the free name.
			err = fmt.Errorf("%w: %s is already sealed; remove it on the daemon's host first",
				errDeployKeyExists, row.DeployKeySecret)
		}
		return "", rollback(err)
	}
	return key.PublicAuthorizedKey, nil
}

// errGitSync marks a sync that git itself failed, as against a missing source
// or an unopenable key, so a face can keep git's output out of its answer.
var errGitSync = errors.New("git")

// errDeployKeyExists refuses a git source whose deploy-key name already holds a
// sealed secret, an orphan a removed source left behind.
var errDeployKeyExists = errors.New("deploy key exists")

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
		s.redirectFlash(w, r, "/sources", verb+" "+name)
	}
}

// handleSourceEgress sets or clears a source's operator-scoped default egress
// port set (Decision 0023) — the web face of `exocomp source egress <name>
// [port...]`. It mirrors handleSourceSetEnabled: operator-gated, no reauth and
// no confirm_token (it seals nothing and removes nothing). parseFormEgress
// validates the ports (1..65535; junk refused) so a bad entry re-renders the
// detail page at 422 with the egress refusal rather than a 500, and an empty
// field clears the declaration.
func (s *Server) handleSourceEgress(w http.ResponseWriter, r *http.Request) {
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

	ports, err := parseFormEgress(r)
	if err != nil {
		s.log.Info("web: source egress refused", "err", err.Error(), "by", ident.token.Name)
		s.sourceEgressDefaultRefusal(w, r, name, sourceEgressRefusal, r.PostFormValue("egress"))
		return
	}
	if _, err := s.store.SetSourceEgressPorts(r.Context(), src.ID, ports); err != nil {
		s.fail(w, r, err)
		return
	}

	if len(ports) == 0 {
		s.log.Info("web: source egress cleared", "source", name, "by", ident.token.Name)
		s.redirectFlash(w, r, "/sources/"+name, "cleared egress default for "+name)
		return
	}
	s.log.Info("web: source egress", "source", name, "ports", joinPorts(ports, ","), "by", ident.token.Name)
	s.redirectFlash(w, r, "/sources/"+name, "set egress default for "+name)
}

// sourceEgressDefaultRefusal re-renders the source detail page at 422 with the
// egress-default form's refusal on its field line and the typed value kept — the
// forms pattern; a refusal never flashes (flash is success-only, write.go).
func (s *Server) sourceEgressDefaultRefusal(w http.ResponseWriter, r *http.Request, name, msg, typed string) {
	vm, err := s.sourceDetailVMFor(w, r, name)
	if err != nil {
		return
	}
	vm.EgressRefusal = msg
	vm.EgressInput = typed
	s.render(w, r, http.StatusUnprocessableEntity, sourceDetail(vm))
}

// handleSourceRunSecrets sets or clears a source's operator-scoped default
// run-secret binding (Decision 0024) — the web face of `exocomp source
// run-secrets <name> [binding]`. It mirrors handleSourceEgress: operator-gated,
// no reauth (it stores a binding name and seals nothing). A binding with no
// pairs re-renders the detail page at 422 with the refusal; an empty field
// clears the default.
func (s *Server) handleSourceRunSecrets(w http.ResponseWriter, r *http.Request) {
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

	binding := strings.TrimSpace(r.PostFormValue("run_secrets"))
	if _, err := s.store.SetSourceRunSecrets(r.Context(), src.ID, binding); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.log.Info("web: source run-secrets refused", "source", name, "by", ident.token.Name)
			s.sourceRunSecretsRefusal(w, r, name, sourceRunSecretsRefusal, binding)
			return
		}
		s.fail(w, r, err)
		return
	}

	if binding == "" {
		s.log.Info("web: source run-secrets cleared", "source", name, "by", ident.token.Name)
		s.redirectFlash(w, r, "/sources/"+name, "cleared run-secrets default for "+name)
		return
	}
	s.log.Info("web: source run-secrets", "source", name, "binding", binding, "by", ident.token.Name)
	s.redirectFlash(w, r, "/sources/"+name, "set run-secrets default for "+name)
}

// sourceRunSecretsRefusal re-renders the source detail page at 422 with the
// run-secrets form's refusal on its field line and the typed value kept.
func (s *Server) sourceRunSecretsRefusal(w http.ResponseWriter, r *http.Request, name, msg, typed string) {
	vm, err := s.sourceDetailVMFor(w, r, name)
	if err != nil {
		return
	}
	vm.RunSecretsRefusal = msg
	vm.RunSecretsInput = typed
	s.render(w, r, http.StatusUnprocessableEntity, sourceDetail(vm))
}

func (s *Server) handleSourceRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !r.Form.Has("confirmed") {
		s.log.Info("web: source rm refused", "source", name, "by", ident.token.Name)
		s.sourceRmRefusal(w, r, name, sourceRemoveConfirmRefusal)
		return
	}
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
	_ = s.purgeSourceCaches(r.Context(), src)
	s.log.Info("web: source rm", "source", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/sources", "removed "+name)
}

// sourceRmRefusal re-renders the source detail page at 422 with the delete
// verb's refusal on its form-status line — the button-verb shape: a refusal
// renders on the surface that holds the verb, never a flash (flash is
// success-only, write.go).
func (s *Server) sourceRmRefusal(w http.ResponseWriter, r *http.Request, name, msg string) {
	vm, err := s.sourceDetailVMFor(w, r, name)
	if err != nil {
		return
	}
	vm.RmRefusal = msg
	s.render(w, r, http.StatusUnprocessableEntity, sourceDetail(vm))
}

func (s *Server) sourceSyncRefusal(w http.ResponseWriter, r *http.Request, name, msg string) {
	vm, err := s.sourceDetailVMFor(w, r, name)
	if err != nil {
		return
	}
	vm.SyncRefusal = msg
	s.render(w, r, http.StatusUnprocessableEntity, sourceDetail(vm))
}

func (s *Server) openSealedSecret(ctx context.Context, name string) ([]byte, error) {
	if s.sealer == nil {
		if s.dataDir != "" {
			env, err := secrets.Open(s.dataDir)
			if err != nil {
				return nil, err
			}
			defer func() { _ = env.Close() }()
			row, err := s.store.GetSecret(ctx, name)
			if err != nil {
				return nil, err
			}
			return env.OpenSecret(secrets.SealedSecret{
				Name:       row.Name,
				Kind:       row.Kind,
				Nonce:      row.Nonce,
				Ciphertext: row.Ciphertext,
			})
		}
		return nil, errors.New("envelope is not available on this surface")
	}
	row, err := s.store.GetSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.sealer.OpenSecret(secrets.SealedSecret{
		Name:       row.Name,
		Kind:       row.Kind,
		Nonce:      row.Nonce,
		Ciphertext: row.Ciphertext,
	})
}

func forgeKnownHosts(ctx context.Context, st *store.Store, host string) (string, func(), error) {
	noop := func() {}
	rows, err := st.ListKnownHosts(ctx)
	if err != nil {
		return "", noop, err
	}
	var forge []store.KnownHost
	for _, kh := range rows {
		if kh.Host == host {
			forge = append(forge, kh)
		}
	}
	if len(forge) == 0 {
		return "", noop, nil
	}
	f, err := os.CreateTemp("", "exocomp-knownhosts-*")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.WriteString(store.RenderKnownHosts(forge)); err != nil {
		_ = f.Close()
		cleanup()
		return "", noop, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", noop, err
	}
	return f.Name(), cleanup, nil
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (s *Server) syncSource(ctx context.Context, name, refOverride string) (source.SyncResult, error) {
	src, err := s.store.SourceByName(ctx, name)
	if err != nil {
		return source.SyncResult{}, err
	}
	if src.Kind != store.SourceGit {
		return source.SyncResult{}, fmt.Errorf("%q is a %s source; only git sources sync", name, src.Kind)
	}

	loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return source.SyncResult{}, fmt.Errorf("source sync: %w", err)
	}

	ref := src.Ref
	if refOverride != "" {
		ref = refOverride
	}
	opts := source.SyncOptions{Root: loc.Root, URL: src.URI, Ref: ref, Signed: src.Signed}
	if src.DeployKeySecret != "" {
		key, err := s.openSealedSecret(ctx, src.DeployKeySecret)
		if err != nil {
			return source.SyncResult{}, fmt.Errorf("source sync: deploy key: %w", err)
		}
		defer zeroBytes(key)
		opts.DeployKeyPEM = key
	}

	if host := source.ForgeHost(src.URI); host != "" {
		khFile, cleanup, err := forgeKnownHosts(ctx, s.store, host)
		if err != nil {
			return source.SyncResult{}, fmt.Errorf("source sync: known_hosts: %w", err)
		}
		defer cleanup()
		opts.KnownHostsFile = khFile
	}
	if src.Signed && src.AllowedSigners != "" {
		f, err := os.CreateTemp("", "exocomp-signers-*")
		if err != nil {
			return source.SyncResult{}, fmt.Errorf("source sync: allowed-signers: %w", err)
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err := f.WriteString(src.AllowedSigners); err != nil {
			_ = f.Close()
			return source.SyncResult{}, fmt.Errorf("source sync: allowed-signers: %w", err)
		}
		if err := f.Close(); err != nil {
			return source.SyncResult{}, fmt.Errorf("source sync: allowed-signers: %w", err)
		}
		opts.AllowedSignersFile = f.Name()
	}

	res, err := source.Sync(ctx, opts)
	if err != nil {
		return source.SyncResult{}, fmt.Errorf("source sync: %w: %w", errGitSync, err)
	}
	if err := s.store.RecordSourceSync(ctx, src.ID, res.Ref, res.SHA); err != nil {
		return source.SyncResult{}, fmt.Errorf("source sync: record: %w", err)
	}
	return res, nil
}

func (s *Server) handleSourceSync(w http.ResponseWriter, r *http.Request) {
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
	if src.Kind != store.SourceGit {
		s.sourceSyncRefusal(w, r, name, fmt.Sprintf("%q is a %s source; only git sources sync", name, src.Kind))
		return
	}

	res, err := s.syncSource(r.Context(), name, "")
	if err != nil {
		s.log.Error("web: source sync failed", "source", name, "err", err.Error(), "by", ident.token.Name)
		s.sourceSyncRefusal(w, r, name, "Sync failed — check the daemon log.")
		return
	}

	s.log.Info("web: source synced", "source", name, "ref", res.Ref, "sha", res.SHA, "by", ident.token.Name)
	flashMsg := fmt.Sprintf("synced %s to %s (%s)", name, res.Ref, shortSHA(res.SHA))

	redirectPath := "/sources/" + name
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			cleanPath := strings.TrimPrefix(u.Path, s.base)
			if cleanPath == "/sources" || cleanPath == "/sources/" {
				redirectPath = "/sources"
			}
		}
	}
	s.redirectFlash(w, r, redirectPath, flashMsg)
}

// purgeSourceCaches removes a deleted source's dependency caches
// (cache/python/<SourceKey> and cache/collections/<SourceKey> under the data
// dir) unless another registered source resolves to the same root and therefore
// shares the key (Decision 0015). A disabled source still owns its cache when
// re-enabled, so it counts as a reference. The source row is already deleted by
// the caller. Any failure leaves the caches in place: the failure mode is a
// stale cache, never a wrong purge.
func (s *Server) purgeSourceCaches(ctx context.Context, src store.Source) bool {
	loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return false
	}
	key := deps.SourceKey(loc.Root)
	rows, err := s.store.ListSources(ctx)
	if err != nil {
		return false
	}
	for _, other := range rows {
		otherLoc, err := source.Resolve(s.dataDir, other.Kind, other.Name, other.URI, other.Writable)
		if err != nil {
			continue
		}
		if deps.SourceKey(otherLoc.Root) == key {
			return false
		}
	}
	if err := deps.RemoveProjectCaches(s.dataDir, loc.Root); err != nil {
		s.log.Warn("web: source rm: purge caches failed", "source", src.Name, "error", err)
	}
	return true
}
