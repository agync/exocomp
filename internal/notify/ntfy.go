package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sealed secret names the ntfy transport is configured from. Nothing here
// reads the environment: a push endpoint that can approve an apply is a
// credential, and credentials live in the envelope.
const (
	SecretNtfyURL   = "ntfy/url"
	SecretNtfyTopic = "ntfy/topic"
	SecretNtfyToken = "ntfy/token"
)

// publicNtfyBase is the public ntfy service. Its topics are unauthenticated
// channels: anyone who guesses the topic can read it and publish to it.
const publicNtfyBase = "ntfy.sh"

// isPublicNtfyHost reports whether host is ntfy.sh or any host under it. It
// normalizes first: url.Hostname does not case-fold ("NTFY.SH") or strip the
// trailing dot of a fully-qualified name ("ntfy.sh."), and either spelling
// reaches the public server while a raw comparison would miss it and wave the
// tokenless configuration through.
func isPublicNtfyHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == publicNtfyBase || strings.HasSuffix(host, "."+publicNtfyBase)
}

// ntfyTimeout bounds one publish. A phone that is not reachable must not hold
// a guardrail decision open.
const ntfyTimeout = 10 * time.Second

// SecretLookup returns a sealed secret's plaintext by name, or an error when
// the secret is absent or will not open.
type SecretLookup func(ctx context.Context, name string) ([]byte, error)

// Ntfy publishes to an ntfy server.
type Ntfy struct {
	base   *url.URL
	topic  string
	token  string
	client *http.Client
}

// NtfyOptions configures the transport.
type NtfyOptions struct {
	// BaseURL is the ntfy server, e.g. https://push.lan.
	BaseURL string
	// Topic is the topic to publish to.
	Topic string
	// Token is an ntfy access token. It is mandatory on a public server.
	Token string
	// Client overrides the HTTP client, for tests.
	Client *http.Client
}

// NewNtfy returns an ntfy transport, refusing a configuration whose approval
// links would be readable by anyone who guessed the topic.
//
// Approval URLs are bearer capabilities to run playbooks against the fleet, so
// a bare ntfy.sh topic is not "obscure", it is unauthenticated: the transport
// defaults to self-hosted, and a public server without an access token is
// refused rather than warned about (platform contract, "Approval callbacks").
func NewNtfy(opts NtfyOptions) (*Ntfy, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("notify: ntfy has no server URL")
	}
	if opts.Topic == "" {
		return nil, errors.New("notify: ntfy has no topic")
	}
	base, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("notify: ntfy server URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("notify: ntfy server URL scheme %q is not http or https", base.Scheme)
	}
	if isPublicNtfyHost(base.Hostname()) && opts.Token == "" {
		return nil, fmt.Errorf("notify: refusing topic %q on the public server %s without an "+
			"access token: approval links are bearer capabilities, and a public topic is "+
			"readable and publishable by anyone who guesses it — self-host ntfy, or seal %s",
			opts.Topic, base.Hostname(), SecretNtfyToken)
	}

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: ntfyTimeout}
	}
	return &Ntfy{base: base, topic: opts.Topic, token: opts.Token, client: client}, nil
}

// LoadNtfy builds the transport from sealed secrets. An absent URL or topic
// means the operator has not configured push at all, which is not an error:
// the caller gets a nil transport and a daemon that runs without one.
func LoadNtfy(ctx context.Context, look SecretLookup) (*Ntfy, error) {
	if look == nil {
		return nil, nil
	}
	base, err := look(ctx, SecretNtfyURL)
	if err != nil {
		return nil, nil
	}
	topic, err := look(ctx, SecretNtfyTopic)
	if err != nil {
		return nil, fmt.Errorf("notify: %s is sealed but %s is not", SecretNtfyURL, SecretNtfyTopic)
	}
	// A missing token is a configuration NewNtfy judges: mandatory on a public
	// server, optional on the operator's own.
	token, _ := look(ctx, SecretNtfyToken)

	return NewNtfy(NtfyOptions{
		BaseURL: strings.TrimSpace(string(base)),
		Topic:   strings.TrimSpace(string(topic)),
		Token:   strings.TrimSpace(string(token)),
	})
}

// Name implements Transport.
func (n *Ntfy) Name() string { return "ntfy" }

// Endpoint reports the topic URL, for doctor output. It carries no token.
func (n *Ntfy) Endpoint() string { return n.base.JoinPath(n.topic).String() }

// ntfyMessage is ntfy's publish document.
type ntfyMessage struct {
	Topic    string       `json:"topic"`
	Title    string       `json:"title,omitempty"`
	Message  string       `json:"message"`
	Priority int          `json:"priority,omitempty"`
	Tags     []string     `json:"tags,omitempty"`
	Actions  []ntfyAction `json:"actions,omitempty"`
}

type ntfyAction struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	URL    string `json:"url"`
	Clear  bool   `json:"clear,omitempty"`
}

// Publish implements Transport.
func (n *Ntfy) Publish(ctx context.Context, note Notification) error {
	msg := ntfyMessage{
		Topic:    n.topic,
		Title:    note.Title,
		Message:  note.Body,
		Priority: int(note.Priority),
		Tags:     note.Tags,
	}
	for _, a := range note.Actions {
		msg.Actions = append(msg.Actions, ntfyAction{
			Action: "view", Label: a.Label, URL: a.URL, Clear: true,
		})
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notify: encode ntfy message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: ntfy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: publish to ntfy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is drained and discarded: it echoes the message back, and an
	// error body could quote a decision URL into a log.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify: ntfy answered %s", resp.Status)
	}
	return nil
}
