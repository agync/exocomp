package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/wire"
)

// maxBody caps a response body read. The read surface is values-free identifier
// lists; a multi-megabyte answer is a misbehaving or wrong endpoint, not a
// legitimate reply, so we refuse to buffer it unbounded.
const maxBody = 8 << 20

// defaultTimeout bounds a single request. A remote read is a small JSON fetch;
// a hung tailnet path should fail a command, not wedge it.
const defaultTimeout = 30 * time.Second

// connectTimeout bounds the TCP dial and TLS handshake only. It applies to
// BOTH hc and stream: stream has no overall Client.Timeout (a follow's body
// read legitimately runs as long as the job does), so without this the
// connect phase to a blackholed address would hang until the OS TCP retry
// gives up, which can be minutes.
const connectTimeout = 10 * time.Second

// Config is what `exocomp login` stored for one remote and what the CLI resolves
// per invocation: where the daemon is, the pinned cert fingerprint, and the
// bearer token. Endpoint may be a bare host, host:port, or a full https URL; it
// is normalized to an https base URL.
type Config struct {
	Endpoint    string
	Fingerprint string
	Token       string
}

// Client is a typed, pinned HTTP client over one daemon's read API. It holds a
// single http.Client whose TLS config pins the daemon's certificate; every
// request carries the bearer token.
type Client struct {
	base *url.URL
	pin  string
	tok  string
	hc   *http.Client
	// stream is the same pinned transport with NO client timeout: an SSE follow
	// is open for as long as the job runs, and defaultTimeout would cut a long
	// apply off mid-run. Its deadline is the caller's context instead.
	stream *http.Client
}

// New builds a client for one remote. It requires an endpoint, a pinned
// fingerprint, and a token: the pin is what makes the connection safe on a bare
// tailnet, and the CLI resolves all three (from hosts.toml / flags / env) before
// calling here.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("client: endpoint is required")
	}
	if strings.TrimSpace(cfg.Fingerprint) == "" {
		return nil, errors.New("client: pinned fingerprint is required (run `exocomp login` first)")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("client: token is required (pass --token, set EXOCOMP_TOKEN, or `exocomp login`)")
	}
	base, err := baseURL(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	c := &Client{base: base, pin: cfg.Fingerprint, tok: cfg.Token}
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: connectTimeout}).DialContext,
		TLSHandshakeTimeout: connectTimeout,
		// Standard chain verification is skipped on purpose: trust is the
		// pinned leaf fingerprint (TOFU, ADR 0009 §4), not a CA. The pin is
		// enforced in VerifyConnection below, which STILL runs when
		// InsecureSkipVerify is set — so a wrong cert is refused there.
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // trust is the pinned fingerprint checked in VerifyConnection
			VerifyConnection:   c.verifyPin,
		},
	}
	c.hc = &http.Client{Timeout: defaultTimeout, Transport: transport}
	c.stream = &http.Client{Transport: transport}
	return c, nil
}

// verifyPin refuses the handshake unless the leaf certificate matches the pin.
func (c *Client) verifyPin(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("client: server presented no certificate")
	}
	got := Fingerprint(cs.PeerCertificates[0].Raw)
	if !sameFingerprint(got, c.pin) {
		return &PinMismatchError{Endpoint: c.base.Host, Expected: c.pin, Got: got}
	}
	return nil
}

// get issues a GET against a /api/v1 path (with optional query), decodes the
// wire success envelope's data into out, and turns a wire error envelope into a
// typed APIError. A non-2xx without a decodable envelope becomes an APIError
// carrying the status so the caller still exits sensibly.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	_, data, err := c.do(ctx, http.MethodGet, path, q, nil, "")
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("client: decode %s payload: %w", path, err)
		}
	}
	return nil
}

// do issues one request against a /api/v1 path and returns the wire success
// envelope's kind and raw data. It is the single request path both the reads
// (get) and the writes (post) run through, so a wire error envelope becomes the
// same typed APIError whichever verb asked. A write needs the KIND back — an
// apply answers either job.enqueued or approval.pending — which is why the
// envelope is not decoded into a caller-chosen type here.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any,
	idempotencyKey string) (string, json.RawMessage, error) {
	u := *c.base
	setPath(&u, path)
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}

	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", nil, fmt.Errorf("client: encode %s body: %w", path, err)
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		return "", nil, fmt.Errorf("client: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		// The header the write API turns into Spec.IdempotencyKey, so a retried
		// request dedups to the one job instead of enqueuing a second.
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// A pin mismatch surfaces here wrapped by net/http; unwrap it so the
		// caller sees the terminal PinMismatchError, not a transport error.
		var pin *PinMismatchError
		if errors.As(err, &pin) {
			return "", nil, pin
		}
		return "", nil, fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", nil, fmt.Errorf("client: read %s: %w", path, err)
	}
	return decodeEnvelope(resp.StatusCode, raw)
}

// decodeEnvelope turns one response into the success envelope's kind and data,
// or a typed error. A wire error envelope keeps its taxonomy code; a non-2xx
// that is not an envelope at all (a proxy page, a bare 404) folds its status
// into the closest code so exit codes stay meaningful.
func decodeEnvelope(status int, raw []byte) (string, json.RawMessage, error) {
	var env struct {
		SchemaVersion int             `json:"schema_version"`
		Kind          string          `json:"kind"`
		Data          json.RawMessage `json:"data"`
		Error         *wire.Error     `json:"error"`
	}
	if derr := json.Unmarshal(raw, &env); derr != nil {
		return "", nil, statusError(status, strings.TrimSpace(string(raw)))
	}
	if env.Kind == wire.KindError || env.Error != nil {
		e := wire.Error{}
		if env.Error != nil {
			e = *env.Error
		}
		return "", nil, &APIError{Status: status, Code: e.Code, Message: e.Message, Hint: e.Hint}
	}
	if status < 200 || status > 299 {
		return "", nil, statusError(status, strings.TrimSpace(string(raw)))
	}
	return env.Kind, env.Data, nil
}

// statusError builds a typed APIError for a non-envelope HTTP failure, mapping
// the status onto the closest taxonomy code so ExitCode still works.
func statusError(status int, body string) *APIError {
	code := wire.CodeInternal
	switch status {
	case http.StatusNotFound:
		code = wire.CodeNotFound
	case http.StatusUnauthorized:
		code = wire.CodeUnauthenticated
	case http.StatusForbidden:
		code = wire.CodeForbidden
	case http.StatusServiceUnavailable:
		code = wire.CodeKDFBusy
	}
	msg := body
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &APIError{Status: status, Code: code, Message: fmt.Sprintf("remote returned HTTP %d: %s", status, msg)}
}

// pathEscape escapes a name being spliced into a path segment, so a workspace
// whose name carries a slash or a space still addresses the row it names
// instead of a different route.
func pathEscape(s string) string { return url.PathEscape(s) }

// setPath installs an already-escaped path on u. Assigning u.Path alone would
// escape it a SECOND time (a %2F becoming %252F), so the escaped form is kept as
// RawPath alongside its decoded Path — the pair net/url emits verbatim.
func setPath(u *url.URL, path string) {
	full := strings.TrimRight(u.Path, "/") + path
	decoded, err := url.PathUnescape(full)
	if err != nil {
		u.Path, u.RawPath = full, ""
		return
	}
	u.Path, u.RawPath = decoded, full
}

// baseURL normalizes an endpoint to an https base URL. A bare host or host:port
// gets an https scheme; an explicit http scheme is refused because a bearer on
// cleartext is sniffable (agent-interface §4).
func baseURL(endpoint string) (*url.URL, error) {
	raw := strings.TrimSpace(endpoint)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("client: parse endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("client: endpoint %q must be https; a bearer over cleartext is sniffable", endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("client: endpoint %q has no host", endpoint)
	}
	return u, nil
}

// hostPort returns the host:port to dial for an endpoint, defaulting to 443 when
// no port is given. It is used by the bare TOFU handshake, which needs an
// address, not a URL.
func hostPort(endpoint string) (string, error) {
	u, err := baseURL(endpoint)
	if err != nil {
		return "", err
	}
	host := u.Host
	if u.Port() == "" {
		host = u.Hostname() + ":443"
	}
	return host, nil
}
