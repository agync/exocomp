package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"exocomp.dev/exocomp/internal/wire"
)

// This file is the typed surface of the source, secret and run-secret routes
// (internal/web/api_admin.go). The shapes mirror the API's views field for field,
// so a decoded answer re-emitted through wire.WriteSuccess reproduces the
// daemon's bytes. A secret value goes out in a request and never comes back.

// Admin response kinds, mirroring internal/web/api_admin.go.
const (
	KindSourceAdded     = "source.added"
	KindSourceRemoved   = "source.removed"
	KindSourceSynced    = "source.synced"
	KindSource          = "source.get"
	KindSecretSealed    = "secret.sealed"
	KindSecretReplaced  = "secret.replaced"
	KindSecretRemoved   = "secret.removed"
	KindRunSecretList   = "run_secret.bindings"
	KindRunSecretPairs  = "run_secret.pairs"
	KindRunSecretBound  = "run_secret.bound"
	KindRunSecretRemove = "run_secret.removed"
)

// SourceAddRequest registers a path source (Path, a tree on the daemon's host)
// or a git source (URL). AllowedSigners is the signers file's content.
type SourceAddRequest struct {
	Name           string `json:"name"`
	Path           string `json:"path,omitempty"`
	URL            string `json:"url,omitempty"`
	Ref            string `json:"ref,omitempty"`
	Writable       bool   `json:"writable,omitempty"`
	Signed         bool   `json:"signed,omitempty"`
	AllowedSigners string `json:"allowed_signers,omitempty"`
}

// SourceAdded is a registered source and, for a git source, the deploy key's
// public half to add to the forge.
type SourceAdded struct {
	Source    wire.SourceView `json:"source"`
	PublicKey string          `json:"public_key,omitempty"`
}

// SourceRemoved names the removed source and whether its caches went with it.
type SourceRemoved struct {
	Name          string `json:"name"`
	CachesRemoved bool   `json:"caches_removed"`
}

// SourceSynced is the pin a sync advanced to.
type SourceSynced struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
}

// SecretAddRequest seals Value, or a minted value with Generate. Value travels
// base64 in JSON, so binary bytes arrive unchanged. ExpiresAt is RFC3339 or
// empty.
type SecretAddRequest struct {
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Value     []byte `json:"value,omitempty"`
	Generate  bool   `json:"generate,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// SecretSealed describes a sealed secret without its value.
type SecretSealed struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Bytes     int    `json:"bytes"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// SecretReplaceRequest is a new value, or Generate. SetExpiry replaces the
// expiry with ExpiresAt; an empty ExpiresAt clears it.
type SecretReplaceRequest struct {
	Value     []byte `json:"value,omitempty"`
	Generate  bool   `json:"generate,omitempty"`
	SetExpiry bool   `json:"set_expiry,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// SecretReplaced says whether the secret was rotated (generated) or replaced.
type SecretReplaced struct {
	Name        string `json:"name"`
	Rotated     bool   `json:"rotated"`
	Regenerated bool   `json:"regenerated"`
	Bytes       int    `json:"bytes"`
	SetExpiry   bool   `json:"set_expiry,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

// SecretRemoved names the deleted secret and how many bindings a forced delete
// left pointing at it.
type SecretRemoved struct {
	Name           string `json:"name"`
	ForcedBindings int    `json:"forced_bindings,omitempty"`
}

// RunSecretPair maps an env name to a sealed secret under a binding.
type RunSecretPair struct {
	Binding string `json:"binding"`
	EnvName string `json:"env_name"`
	Secret  string `json:"secret"`
}

// call sends one admin request and decodes the success data, checking the kind.
func (c *Client) call(ctx context.Context, method, path string, q url.Values, body any, want string, out any) error {
	kind, data, err := c.do(ctx, method, path, q, body, "")
	if err != nil {
		return err
	}
	if kind != want {
		return fmt.Errorf("client: %s %s answered with unexpected kind %q", method, path, kind)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("client: decode %s payload: %w", kind, err)
	}
	return nil
}

// AddSource registers a source on the daemon.
func (c *Client) AddSource(ctx context.Context, req SourceAddRequest) (SourceAdded, error) {
	var out SourceAdded
	err := c.call(ctx, http.MethodPost, "/api/v1/sources", nil, req, KindSourceAdded, &out)
	return out, err
}

// RemoveSource removes a source.
func (c *Client) RemoveSource(ctx context.Context, name string) (SourceRemoved, error) {
	var out SourceRemoved
	err := c.call(ctx, http.MethodDelete, "/api/v1/sources/"+pathEscape(name), nil, nil, KindSourceRemoved, &out)
	return out, err
}

// SyncSource clones or fetches a git source on the daemon; ref overrides the
// pinned ref when not empty.
func (c *Client) SyncSource(ctx context.Context, name, ref string) (SourceSynced, error) {
	var out SourceSynced
	err := c.call(ctx, http.MethodPost, "/api/v1/sources/"+pathEscape(name)+"/sync", nil,
		map[string]string{"ref": ref}, KindSourceSynced, &out)
	return out, err
}

// SetSourceEgress replaces a source's default egress ports; none clears them.
func (c *Client) SetSourceEgress(ctx context.Context, name string, ports []int) (wire.SourceView, error) {
	if ports == nil {
		ports = []int{}
	}
	var out wire.SourceView
	err := c.call(ctx, http.MethodPut, "/api/v1/sources/"+pathEscape(name)+"/egress", nil,
		map[string][]int{"ports": ports}, KindSource, &out)
	return out, err
}

// SetSourceRunSecrets sets a source's default run-secret binding; empty clears
// it.
func (c *Client) SetSourceRunSecrets(ctx context.Context, name, binding string) (wire.SourceView, error) {
	var out wire.SourceView
	err := c.call(ctx, http.MethodPut, "/api/v1/sources/"+pathEscape(name)+"/run-secrets", nil,
		map[string]string{"binding": binding}, KindSource, &out)
	return out, err
}

// SealSecret seals a new secret on the daemon.
func (c *Client) SealSecret(ctx context.Context, req SecretAddRequest) (SecretSealed, error) {
	var out SecretSealed
	err := c.call(ctx, http.MethodPost, "/api/v1/secrets", nil, req, KindSecretSealed, &out)
	return out, err
}

// ReplaceSecret reseals an existing secret.
func (c *Client) ReplaceSecret(ctx context.Context, name string, req SecretReplaceRequest) (SecretReplaced, error) {
	var out SecretReplaced
	err := c.call(ctx, http.MethodPut, "/api/v1/secrets/"+pathEscape(name), nil, req, KindSecretReplaced, &out)
	return out, err
}

// RemoveSecret deletes a sealed secret. Without force, one a binding still maps
// is a conflict APIError naming the bindings.
func (c *Client) RemoveSecret(ctx context.Context, name string, force bool) (SecretRemoved, error) {
	var q url.Values
	if force {
		q = url.Values{"force": {"true"}}
	}
	var out SecretRemoved
	err := c.call(ctx, http.MethodDelete, "/api/v1/secrets/"+pathEscape(name), q, nil, KindSecretRemoved, &out)
	return out, err
}

// RunSecretBindings lists the binding names.
func (c *Client) RunSecretBindings(ctx context.Context) ([]string, error) {
	var out []string
	err := c.call(ctx, http.MethodGet, "/api/v1/run-secrets", nil, nil, KindRunSecretList, &out)
	return out, err
}

// RunSecretPairs lists the pairs under one binding.
func (c *Client) RunSecretPairs(ctx context.Context, binding string) ([]RunSecretPair, error) {
	var out []RunSecretPair
	err := c.call(ctx, http.MethodGet, "/api/v1/run-secrets/"+pathEscape(binding), nil, nil, KindRunSecretPairs, &out)
	return out, err
}

// BindRunSecret maps envName to the sealed secret under binding.
func (c *Client) BindRunSecret(ctx context.Context, binding, envName, secret string) (RunSecretPair, error) {
	var out RunSecretPair
	err := c.call(ctx, http.MethodPost, "/api/v1/run-secrets/"+pathEscape(binding), nil,
		map[string]string{"env_name": envName, "secret": secret}, KindRunSecretBound, &out)
	return out, err
}

// RemoveRunSecretBinding removes every pair under a binding.
func (c *Client) RemoveRunSecretBinding(ctx context.Context, binding string) error {
	return c.call(ctx, http.MethodDelete, "/api/v1/run-secrets/"+pathEscape(binding), nil, nil, KindRunSecretRemove, nil)
}
