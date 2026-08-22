package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sealed secret names the GoAlert transport is configured from, mirroring the
// ntfy loader: nothing here reads the environment. The integration key can open
// an incident that pages an on-call rotation, so it is a credential and lives in
// the envelope, never in a flag or a file.
const (
	SecretGoAlertURL       = "goalert/url"
	SecretGoAlertKey       = "goalert/key"
	SecretGoAlertNamespace = "goalert/namespace"
)

// goalertTimeout bounds one publish. GoAlert being slow or unreachable must not
// hold a guardrail decision open, exactly as for ntfy.
const goalertTimeout = 10 * time.Second

// GoAlert publishes to a GoAlert Generic API integration key.
type GoAlert struct {
	base      *url.URL
	key       string
	namespace string
	client    *http.Client
}

// GoAlertOptions configures the transport.
type GoAlertOptions struct {
	// BaseURL is the GoAlert server, e.g. https://goalert.lan.
	BaseURL string
	// Key is the Generic API integration key. It is mandatory.
	Key string
	// Namespace optionally prefixes every dedup key, so two exocomp instances
	// pointing at one GoAlert integration do not collide on the same incident.
	Namespace string
	// Client overrides the HTTP client, for tests.
	Client *http.Client
}

// NewGoAlert returns a GoAlert transport.
func NewGoAlert(opts GoAlertOptions) (*GoAlert, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("notify: goalert has no server URL")
	}
	if opts.Key == "" {
		return nil, errors.New("notify: goalert has no integration key")
	}
	base, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("notify: goalert server URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("notify: goalert server URL scheme %q is not http or https", base.Scheme)
	}

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: goalertTimeout}
	}
	return &GoAlert{base: base, key: opts.Key, namespace: opts.Namespace, client: client}, nil
}

// LoadGoAlert builds the transport from sealed secrets, mirroring LoadNtfy. An
// absent URL means the operator has not configured GoAlert at all, which is not
// an error: the caller gets a nil transport and a daemon that runs without it,
// so an operator who has not set GoAlert up still gets a working, silent daemon.
func LoadGoAlert(ctx context.Context, look SecretLookup) (*GoAlert, error) {
	if look == nil {
		return nil, nil
	}
	base, err := look(ctx, SecretGoAlertURL)
	if err != nil {
		return nil, nil
	}
	key, err := look(ctx, SecretGoAlertKey)
	if err != nil {
		return nil, fmt.Errorf("notify: %s is sealed but %s is not", SecretGoAlertURL, SecretGoAlertKey)
	}
	// A namespace is optional; its absence is not a half-finished setup.
	ns, _ := look(ctx, SecretGoAlertNamespace)

	return NewGoAlert(GoAlertOptions{
		BaseURL:   strings.TrimSpace(string(base)),
		Key:       strings.TrimSpace(string(key)),
		Namespace: strings.TrimSpace(string(ns)),
	})
}

// Name implements Transport.
func (g *GoAlert) Name() string { return "goalert" }

// Endpoint reports the incoming URL, for doctor output. It carries no token.
func (g *GoAlert) Endpoint() string {
	return g.base.JoinPath("api", "v2", "generic", "incoming").String()
}

// goalertPayload is GoAlert's generic-integration document. A dedup that is
// stable across the firing and the resolve is what lets action=close find the
// incident action=open raised.
type goalertPayload struct {
	Summary string `json:"summary"`
	Details string `json:"details,omitempty"`
	Dedup   string `json:"dedup,omitempty"`
	Action  string `json:"action"`
}

// Publish implements Transport. It opens an incident for a firing notification
// and closes the matching one for a resolved notification, keyed by a dedup
// derived from the notification's Source and Host so the two halves line up.
func (g *GoAlert) Publish(ctx context.Context, n Notification) error {
	action := "open"
	if n.Resolved {
		action = "close"
	}
	payload := goalertPayload{
		Summary: n.Title,
		Details: n.Body,
		Dedup:   g.dedup(n),
		Action:  action,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notify: encode goalert payload: %w", err)
	}

	endpoint := g.base.JoinPath("api", "v2", "generic", "incoming")
	endpoint.RawQuery = url.Values{"token": {g.key}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		// The URL carries the integration key, so a NewRequest error — which
		// would quote that URL — is deliberately not wrapped.
		return errors.New("notify: build goalert request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		// A transport error is a *url.Error carrying the full request URL, and
		// that query string is the integration key: strip it before it reaches a
		// log or an error string (AGENTS.md invariant 4).
		return fmt.Errorf("notify: publish to goalert: %w", stripToken(err))
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is drained and discarded: an error body could quote the request
	// back, key and all.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify: goalert answered %s", resp.Status)
	}
	return nil
}

// dedup returns the incident key for a notification: the optional namespace
// prefix over a stable key derived from what the notification is about.
func (g *GoAlert) dedup(n Notification) string {
	key := goalertDedup(n)
	if g.namespace != "" {
		return g.namespace + ":" + key
	}
	return key
}

// goalertDedup derives a stable dedup key from a notification's attribution, so
// the resolve of a condition closes the incident its firing opened. Source and
// Host are what a scoped mute already keys on; when a notification carries
// neither, its title is hashed so repeats still collapse to one incident.
func goalertDedup(n Notification) string {
	switch {
	case n.Source != "" && n.Host != "":
		return n.Source + "/" + n.Host
	case n.Source != "":
		return n.Source
	case n.Host != "":
		return n.Host
	default:
		sum := sha256.Sum256([]byte(n.Title))
		return "t:" + hex.EncodeToString(sum[:8])
	}
}

// stripToken removes the request URL — which carries the integration key in its
// query — from a transport error. client.Do returns a *url.Error wrapping the
// URL; its own cause (the dial or timeout) names no secret.
func stripToken(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}
