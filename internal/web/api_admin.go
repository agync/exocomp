package web

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/naming"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// This file mounts the /api/v1 routes for the source, secret and run-secret
// registrations (Decision 0027): what `exocomp source`, `secrets` and
// `run-secret` do on the daemon's own host. Every route takes the operator scope
// as a bearer, like the enqueuing writes, and none needs the browser's reauth:
// that step proves possession for an ambient cookie session, and a bearer is the
// possession proof itself. A sealed value travels in a request body and never
// in a response; a git source's deploy key answers with its public half once.

// Admin response kinds. Like the write kinds, they carry names, states and
// sizes, never a value (invariant 4).
const (
	kindSourceAdded     = "source.added"
	kindSourceRemoved   = "source.removed"
	kindSourceSynced    = "source.synced"
	kindSource          = "source.get"
	kindSecretSealed    = "secret.sealed"
	kindSecretReplaced  = "secret.replaced"
	kindSecretRemoved   = "secret.removed"
	kindRunSecretList   = "run_secret.bindings"
	kindRunSecretPairs  = "run_secret.pairs"
	kindRunSecretBound  = "run_secret.bound"
	kindRunSecretRemove = "run_secret.removed"
)

func (s *Server) registerAdminAPI(mux *http.ServeMux) {
	write := func(what string, h http.HandlerFunc) http.HandlerFunc {
		return s.apiGuard(bearerOnly(canWrite), writeDeny(what, "operator"), h)
	}
	mux.HandleFunc("POST /api/v1/sources", write("manage sources", s.apiSourceAdd))
	mux.HandleFunc("DELETE /api/v1/sources/{name}", write("manage sources", s.apiSourceRm))
	mux.HandleFunc("POST /api/v1/sources/{name}/sync", write("manage sources", s.apiSourceSync))
	mux.HandleFunc("PUT /api/v1/sources/{name}/egress", write("manage sources", s.apiSourceEgress))
	mux.HandleFunc("PUT /api/v1/sources/{name}/run-secrets", write("manage sources", s.apiSourceRunSecrets))
	mux.HandleFunc("POST /api/v1/secrets", write("manage secrets", s.apiSecretAdd))
	mux.HandleFunc("PUT /api/v1/secrets/{name...}", write("manage secrets", s.apiSecretReplace))
	mux.HandleFunc("DELETE /api/v1/secrets/{name...}", write("manage secrets", s.apiSecretRm))
	mux.HandleFunc("GET /api/v1/run-secrets", s.apiAuth(s.apiRunSecretBindings))
	mux.HandleFunc("GET /api/v1/run-secrets/{binding}", s.apiAuth(s.apiRunSecretPairs))
	mux.HandleFunc("POST /api/v1/run-secrets/{binding}", write("manage run-secrets", s.apiRunSecretAdd))
	mux.HandleFunc("DELETE /api/v1/run-secrets/{binding}", write("manage run-secrets", s.apiRunSecretRm))
}

// maxAdminBody bounds an admin request: a secret value at its ceiling, base64
// encoded, plus room for the other fields.
const maxAdminBody = 2*service.MaxSecretValueBytes + 1<<16

// decodeBody decodes an admin route's body under decodeJSON's rules at the
// admin size limit. A misspelt field must not be dropped quietly: an "expires"
// for "expires_at" would seal a secret with no expiry.
func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return s.decodeJSON(w, r, v, maxAdminBody)
}

// checkPathSource refuses at add time a path source no job could run from: a
// relative path that leaves the data volume, or a tree that covers the data
// dir or its state files. The sandbox refuses such a source at run time anyway;
// refusing it here keeps a source that can never run out of the registry.
func (s *Server) checkPathSource(row store.Source) error {
	if s.dataDir == "" {
		return nil
	}
	loc, err := source.Resolve(s.dataDir, row.Kind, row.Name, row.URI, row.Writable)
	if err != nil {
		return err
	}
	return executor.SandboxProfile{DataDir: s.dataDir, Read: []string{loc.Root}}.Validate()
}

// adminFail maps a refusal the store or the service verbs return onto the
// taxonomy: the caller's own bad input is a 400, the rest as apiFail maps it.
func (s *Server) adminFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrValidation), errors.Is(err, store.ErrInvalidEgressPort),
		errors.Is(err, service.ErrEmptySecretValue), errors.Is(err, service.ErrInvalidEnvName),
		errors.Is(err, service.ErrSourcePathEscape):
		s.badRequest(w, strings.TrimPrefix(err.Error(), "store: validation: "))
	default:
		s.apiFail(w, err)
	}
}

// requireSealer answers 503 when this process holds no envelope to seal with.
func (s *Server) requireSealer(w http.ResponseWriter) bool {
	if s.sealer == nil {
		s.apiError(w, http.StatusServiceUnavailable,
			wire.NewError(wire.CodeInternal, "sealing is not available on this surface (this process holds no envelope)"))
		return false
	}
	return true
}

// sourceView projects one source the way GET /sources lists it.
func sourceView(src store.Source) wire.SourceView {
	return wire.NewSourceList([]store.Source{src})[0]
}

// --- sources ---------------------------------------------------------------

type sourceAddRequest struct {
	Name           string `json:"name"`
	Path           string `json:"path"`
	URL            string `json:"url"`
	Ref            string `json:"ref"`
	Writable       bool   `json:"writable"`
	Signed         bool   `json:"signed"`
	AllowedSigners string `json:"allowed_signers"`
}

type sourceAddedView struct {
	Source wire.SourceView `json:"source"`
	// PublicKey is a git source's deploy key in authorized_keys form, to add to
	// the forge as a read-only key. It is the public half only, returned once.
	PublicKey string `json:"public_key,omitempty"`
}

// apiSourceAdd registers a path source (a tree on the daemon's host) or a git
// source, whose deploy key it generates and seals.
func (s *Server) apiSourceAdd(w http.ResponseWriter, r *http.Request) {
	var req sourceAddRequest
	if !s.decodeBody(w, r, &req) {
		return
	}
	if err := naming.ValidateComponent("source", req.Name); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if (req.URL == "") == (req.Path == "") {
		s.badRequest(w, "give exactly one of url or path")
		return
	}
	if req.Signed != (req.AllowedSigners != "") {
		s.badRequest(w, "signed requires allowed_signers, and allowed_signers requires signed")
		return
	}
	switch {
	case req.Path != "" && req.Ref != "":
		s.badRequest(w, "ref applies to a git source")
		return
	case req.URL != "" && req.Writable:
		s.badRequest(w, "writable applies to a path source")
		return
	case req.URL != "" && !source.LooksLikeGitURL(req.URL):
		s.badRequest(w, "url "+req.URL+" is not a git URL; a tree on the daemon's host is a path source")
		return
	case !source.ValidRef(req.Ref):
		s.badRequest(w, "ref "+req.Ref+" is not a git ref")
		return
	}
	row := store.Source{Name: req.Name, Signed: req.Signed, AllowedSigners: req.AllowedSigners, Enabled: true}

	if req.Path != "" {
		// A relative path stays relative so it resolves against the data volume
		// at run time, as on the CLI.
		row.Kind, row.URI, row.Writable = store.SourcePath, filepath.Clean(req.Path), req.Writable
		if err := s.checkPathSource(row); err != nil {
			s.badRequest(w, strings.TrimPrefix(err.Error(), "executor: sandbox: "))
			return
		}
		created, err := s.store.CreateSource(r.Context(), row)
		if err != nil {
			s.adminFail(w, err)
			return
		}
		s.log.Info("api: source add-path", "source", req.Name, "writable", req.Writable, "by", actorFromRequest(r).Name)
		s.apiSuccess(w, kindSourceAdded, sourceAddedView{Source: sourceView(created)})
		return
	}

	if !s.requireSealer(w) {
		return
	}
	row.URI, row.Ref = req.URL, req.Ref
	if row.Ref == "" {
		row.Ref = "main"
	}
	publicKey, err := s.createGitSource(r.Context(), row)
	if errors.Is(err, errDeployKeyExists) {
		s.apiError(w, http.StatusConflict, wire.NewError(wire.CodeConflict, err.Error()))
		return
	}
	if err != nil {
		s.adminFail(w, err)
		return
	}
	created, err := s.store.SourceByName(r.Context(), req.Name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.log.Info("api: source add-git", "source", req.Name, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindSourceAdded, sourceAddedView{Source: sourceView(created), PublicKey: publicKey})
}

type sourceRemovedView struct {
	Name          string `json:"name"`
	CachesRemoved bool   `json:"caches_removed"`
}

// apiSourceRm removes a source; its inventories cascade, a git deploy key stays
// sealed, and its dependency caches go unless another source shares them.
func (s *Server) apiSourceRm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	src, err := s.store.SourceByName(r.Context(), name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if err := s.store.DeleteSource(r.Context(), name); err != nil {
		s.adminFail(w, err)
		return
	}
	purged := s.purgeSourceCaches(r.Context(), src)
	s.log.Info("api: source rm", "source", name, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindSourceRemoved, sourceRemovedView{Name: name, CachesRemoved: purged})
}

type sourceSyncedView struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
}

// apiSourceSync clones or fetches a git source and advances its pin, the same
// inline sync the browser's Sync button runs. Body: {ref} to override the ref.
func (s *Server) apiSourceSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
	}
	if !s.decodeBody(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	src, err := s.store.SourceByName(r.Context(), name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if src.Kind != store.SourceGit {
		s.badRequest(w, src.Name+" is a "+src.Kind+" source; only git sources sync (the tree is already present)")
		return
	}
	if !source.ValidRef(req.Ref) {
		s.badRequest(w, "ref "+req.Ref+" is not a git ref")
		return
	}
	res, err := s.syncSource(r.Context(), name, req.Ref)
	if errors.Is(err, errGitSync) {
		// git's own output can carry the forge's paths and messages; it goes to
		// the daemon log, as the browser's Sync does.
		s.log.Error("api: source sync failed", "source", name, "err", err.Error(), "by", actorFromRequest(r).Name)
		s.apiError(w, http.StatusBadGateway, wire.NewError(wire.CodeInternal,
			"sync of "+name+" failed; the daemon log has git's output"))
		return
	}
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.log.Info("api: source synced", "source", name, "ref", res.Ref, "sha", res.SHA, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindSourceSynced, sourceSyncedView{Name: name, Ref: res.Ref, SHA: res.SHA})
}

// apiSourceEgress replaces a source's default egress set (Decision 0023); an
// empty list clears it. Body: {ports}.
func (s *Server) apiSourceEgress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ports []int `json:"ports"`
	}
	if !s.decodeBody(w, r, &req) {
		return
	}
	src, err := s.store.SourceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	updated, err := s.store.SetSourceEgressPorts(r.Context(), src.ID, req.Ports)
	if err != nil {
		s.adminFail(w, err)
		return
	}
	s.log.Info("api: source egress", "source", src.Name, "ports", joinPorts(updated.ExtraEgressPorts, ","),
		"by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindSource, sourceView(updated))
}

// apiSourceRunSecrets sets or clears a source's default run-secret binding
// (Decision 0024). Body: {binding}; empty clears it.
func (s *Server) apiSourceRunSecrets(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Binding string `json:"binding"`
	}
	if !s.decodeBody(w, r, &req) {
		return
	}
	binding := strings.TrimSpace(req.Binding)
	if !s.allowBindingOverAPI(w, binding) {
		return
	}
	src, err := s.store.SourceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	updated, err := s.store.SetSourceRunSecrets(r.Context(), src.ID, binding)
	if errors.Is(err, store.ErrNotFound) {
		s.badRequest(w, "binding "+binding+" has no pairs (add one with `exocomp run-secret add`)")
		return
	}
	if err != nil {
		s.adminFail(w, err)
		return
	}
	s.log.Info("api: source run-secrets", "source", src.Name, "binding", updated.DefaultRunSecrets,
		"by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindSource, sourceView(updated))
}

// --- secrets ---------------------------------------------------------------

type secretAddRequest struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Value is the secret's bytes, base64 in JSON, so a binary value arrives
	// unchanged.
	Value    []byte `json:"value"`
	Generate bool   `json:"generate"`
	// ExpiresAt is an RFC3339 instant; the client turns a relative --expires
	// into one, so the deadline does not depend on this host's clock.
	ExpiresAt string `json:"expires_at"`
}

type secretSealedView struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Bytes     int    `json:"bytes"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// parseExpiresAt reads an optional RFC3339 instant.
func parseExpiresAt(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, errors.New("expires_at is not an RFC3339 timestamp")
	}
	u := t.UTC()
	return &u, nil
}

func formatExpiresAt(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// apiSecretAdd seals a value from the request body, or mints one, into the
// envelope.
func (s *Server) apiSecretAdd(w http.ResponseWriter, r *http.Request) {
	var req secretAddRequest
	if !s.decodeBody(w, r, &req) {
		return
	}
	if !validSecretName(req.Name) {
		s.badRequest(w, "secret name "+req.Name+" is not letters, digits, dots, hyphens, underscores and slashes")
		return
	}
	if req.Kind == "" {
		req.Kind = "generic"
	}
	if service.IsSystemSecret(req.Name, req.Kind) {
		s.refuseTransportKey(w, req.Name, "sealed")
		return
	}
	expiresAt, err := parseExpiresAt(req.ExpiresAt)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if !s.requireSealer(w) {
		return
	}
	res, err := s.service().SealSecret(r.Context(), s.sealer, service.SealSecretInput{
		Name: req.Name, Kind: req.Kind, Value: req.Value, Generate: req.Generate, ExpiresAt: expiresAt,
	})
	if err != nil {
		s.adminFail(w, err)
		return
	}
	s.log.Info("api: secret add", "secret", req.Name, "kind", req.Kind, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindSecretSealed, secretSealedView{
		Name: res.Row.Name, Kind: res.Row.Kind, Source: res.Row.Source, Bytes: res.Bytes,
		ExpiresAt: formatExpiresAt(res.Row.ExpiresAt),
	})
}

type secretReplaceRequest struct {
	Value    []byte `json:"value"`
	Generate bool   `json:"generate"`
	// SetExpiry replaces the expiry with ExpiresAt; an empty ExpiresAt clears it.
	SetExpiry bool   `json:"set_expiry"`
	ExpiresAt string `json:"expires_at"`
}

type secretReplacedView struct {
	Name        string `json:"name"`
	Rotated     bool   `json:"rotated"`
	Regenerated bool   `json:"regenerated"`
	Bytes       int    `json:"bytes"`
	SetExpiry   bool   `json:"set_expiry,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

// apiSecretReplace reseals a secret: a generated one is minted again, a user
// one takes the value from the body.
func (s *Server) apiSecretReplace(w http.ResponseWriter, r *http.Request) {
	var req secretReplaceRequest
	if !s.decodeBody(w, r, &req) {
		return
	}
	expiresAt, err := parseExpiresAt(req.ExpiresAt)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if !s.requireSealer(w) {
		return
	}
	name := r.PathValue("name")
	if !s.allowOverAPI(w, r, name, "replaced") {
		return
	}
	res, err := s.service().ReplaceSecret(r.Context(), s.sealer, service.ReplaceSecretInput{
		Name: name, Value: req.Value, Generate: req.Generate, SetExpiry: req.SetExpiry, ExpiresAt: expiresAt,
	})
	if err != nil {
		s.adminFail(w, err)
		return
	}
	s.log.Info("api: secret replace", "secret", name, "rotated", res.Rotated, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindSecretReplaced, secretReplacedView{
		Name: res.Name, Rotated: res.Rotated, Regenerated: res.Regenerated, Bytes: res.Bytes,
		SetExpiry: res.SetExpiry, ExpiresAt: formatExpiresAt(res.ExpiresAt),
	})
}

type secretRemovedView struct {
	Name           string `json:"name"`
	ForcedBindings int    `json:"forced_bindings,omitempty"`
}

// apiSecretRm deletes a sealed secret. One that a run-secret binding still maps
// is a 409 naming the bindings, unless ?force=true.
func (s *Server) apiSecretRm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.allowOverAPI(w, r, name, "removed") {
		return
	}
	refs, err := s.service().SecretBindings(r.Context(), name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if len(refs) > 0 && r.URL.Query().Get("force") != "true" {
		var b strings.Builder
		for i, ref := range refs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("binding " + ref.Binding + " env " + ref.EnvName)
		}
		s.apiError(w, http.StatusConflict, wire.NewError(wire.CodeConflict,
			"secret "+name+" is still bound: "+b.String()+"; pass force to delete it anyway"))
		return
	}
	if err := s.store.DeleteSecret(r.Context(), name); err != nil {
		s.apiFail(w, err)
		return
	}
	s.log.Info("api: secret rm", "secret", name, "forced_bindings", len(refs), "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindSecretRemoved, secretRemovedView{Name: name, ForcedBindings: len(refs)})
}

// allowOverAPI refuses, with a 403, a secret that is or would stand in for one
// exocomp makes for itself (service.IsSystemSecret): binding it would hand a
// value no operator held to a run, and replacing or removing it would break or
// free a name exocomp fills. All of that stays on the daemon's host with
// --local. A missing secret under a name that is not
// reserved passes, for the verb to report; a store error refuses.
func (s *Server) allowOverAPI(w http.ResponseWriter, r *http.Request, name, verb string) bool {
	kind := ""
	row, err := s.store.GetSecret(r.Context(), name)
	switch {
	case err == nil:
		kind = row.Kind
	case !errors.Is(err, store.ErrNotFound):
		s.apiFail(w, err)
		return false
	}
	if !service.IsSystemSecret(name, kind) {
		return true
	}
	s.refuseTransportKey(w, name, verb)
	return false
}

func (s *Server) refuseTransportKey(w http.ResponseWriter, name, verb string) {
	s.apiError(w, http.StatusForbidden, wire.NewError(wire.CodeForbidden,
		"secret "+name+" is one exocomp makes for itself; it is "+verb+
			" on the daemon's host with --local, never over the API"))
}

// allowBindingOverAPI refuses, with a 403, a binding exocomp collects sealed
// outputs under (service.IsSystemBinding), which a dependent step resolves by
// name.
func (s *Server) allowBindingOverAPI(w http.ResponseWriter, binding string) bool {
	if !service.IsSystemBinding(binding) {
		return true
	}
	s.apiError(w, http.StatusForbidden, wire.NewError(wire.CodeForbidden,
		"binding "+binding+" is one exocomp fills with sealed outputs; change it on the daemon's host with --local"))
	return false
}

// --- run-secret bindings ---------------------------------------------------

type runSecretPairView struct {
	Binding string `json:"binding"`
	EnvName string `json:"env_name"`
	Secret  string `json:"secret"`
}

func (s *Server) apiRunSecretBindings(w http.ResponseWriter, r *http.Request) {
	names, err := s.store.ListRunSecretBindings(r.Context())
	if err != nil {
		s.apiFail(w, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	s.apiSuccess(w, kindRunSecretList, names)
}

func (s *Server) apiRunSecretPairs(w http.ResponseWriter, r *http.Request) {
	pairs, err := s.store.RunSecretsByBinding(r.Context(), r.PathValue("binding"))
	if err != nil {
		s.apiFail(w, err)
		return
	}
	out := make([]runSecretPairView, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, runSecretPairView{Binding: p.Binding, EnvName: p.EnvName, Secret: p.SecretName})
	}
	s.apiSuccess(w, kindRunSecretPairs, out)
}

// apiRunSecretAdd maps an env name to a sealed secret under a binding. Body:
// {env_name, secret}. The pair names the secret; no value is read or returned.
func (s *Server) apiRunSecretAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnvName string `json:"env_name"`
		Secret  string `json:"secret"`
	}
	if !s.decodeBody(w, r, &req) {
		return
	}
	binding := r.PathValue("binding")
	if !s.allowBindingOverAPI(w, binding) || !s.allowOverAPI(w, r, req.Secret, "bound") {
		return
	}
	pair, err := s.service().BindRunSecret(r.Context(), binding, req.EnvName, req.Secret)
	if err != nil {
		s.adminFail(w, err)
		return
	}
	s.log.Info("api: run-secret add", "binding", binding, "env", pair.EnvName, "secret", pair.SecretName,
		"by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindRunSecretBound, runSecretPairView{Binding: pair.Binding, EnvName: pair.EnvName, Secret: pair.SecretName})
}

// apiRunSecretRm removes every pair under a binding. A binding a source names as
// its default is refused until that default is cleared.
func (s *Server) apiRunSecretRm(w http.ResponseWriter, r *http.Request) {
	binding := r.PathValue("binding")
	if !s.allowBindingOverAPI(w, binding) {
		return
	}
	if err := s.store.DeleteRunSecretBinding(r.Context(), binding); err != nil {
		s.adminFail(w, err)
		return
	}
	s.log.Info("api: run-secret rm", "binding", binding, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindRunSecretRemove, map[string]string{"binding": binding})
}
