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

// IsPublicNtfyHost reports whether host is ntfy.sh or any host under it. It
// normalizes first: url.Hostname does not case-fold ("NTFY.SH") or strip the
// trailing dot of a fully-qualified name ("ntfy.sh."), and either spelling
// reaches the public server while a raw comparison would miss it and wave the
// tokenless configuration through.
func IsPublicNtfyHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == publicNtfyBase || strings.HasSuffix(host, "."+publicNtfyBase)
}

func isPublicNtfyHost(host string) bool {
	return IsPublicNtfyHost(host)
}

// ntfyTimeout bounds one publish. A phone that is not reachable must not hold
// a guardrail decision open.
const ntfyTimeout = 10 * time.Second

// Detail controls how much an ntfy push names and whether it carries the
// Approve/Reject action buttons. A push transits a relay — third-party or
// self-hosted — that exocomp does not control, so the ladder trades usefulness
// against what an unread relay and a lock screen can see. It is modelled on
// Android's lock-screen notification setting: show all, hide sensitive content,
// show nothing.
type Detail string

const (
	// DetailFull is the richest and the default: the notification's own title
	// and body — the workspace, host, and error the builders compose — plus the
	// Approve/Reject action buttons.
	DetailFull Detail = "full"
	// DetailSummary hides the sensitive content: a fixed product title and a
	// one-line "what kind of thing needs attention", with NO action buttons —
	// the Approve/Reject capability stays out of an unread relay and off a lock
	// screen, and the operator opens the app to act.
	DetailSummary Detail = "summary"
	// DetailTerse names nothing at all: a fixed product title and body, no
	// buttons.
	DetailTerse Detail = "terse"
)

// terseTitle and terseBody are the fixed strings a DetailTerse push carries.
// They identify the product, not the condition: what needs attention is
// behind the tap.
const (
	terseTitle = "exocomp"
	terseBody  = "exocomp: action required"
)

// normalizeDetail resolves an operator-supplied Detail to a valid one. Unset or
// unrecognized — a stale build, a typo in the setting table, the retired
// "descriptive" — falls back to DetailFull, the documented default: a free-text
// setting must not be able to silently pick a stricter behavior, or silently
// escape one, by accident.
func normalizeDetail(d Detail) Detail {
	switch d {
	case DetailTerse:
		return DetailTerse
	case DetailSummary:
		return DetailSummary
	default:
		return DetailFull
	}
}

// SecretLookup returns a sealed secret's plaintext by name, or an error when
// the secret is absent or will not open.
type SecretLookup func(ctx context.Context, name string) ([]byte, error)

// Ntfy publishes to an ntfy server.
type Ntfy struct {
	base   *url.URL
	topic  string
	token  string
	client *http.Client
	detail Detail
	// publicURL roots a notification's deep link at the deployment's public
	// address, so a tap opens the app instead of an unhandled exocomp://
	// scheme. Empty (the operator set no public URL) leaves the deep link as
	// the builder wrote it.
	publicURL string
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
	// Detail controls how much a push names and whether it carries the action
	// buttons. Empty behaves as DetailFull.
	Detail Detail
	// PublicURL is the deployment's public address (store.SettingPublicURL),
	// e.g. https://exocomp.example/exocomp. It roots a notification's deep
	// link; empty leaves the link unresolved.
	PublicURL string
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
	return &Ntfy{
		base: base, topic: opts.Topic, token: opts.Token, client: client,
		detail: normalizeDetail(opts.Detail), publicURL: strings.TrimRight(strings.TrimSpace(opts.PublicURL), "/"),
	}, nil
}

// SetDetail overrides how much a push names, after construction. LoadNtfy
// builds the transport from sealed secrets alone, so a caller that also has
// the operator's Detail setting (store.SettingNtfyDetail) applies it here.
func (n *Ntfy) SetDetail(d Detail) { n.detail = normalizeDetail(d) }

// SetPublicURL overrides the deployment's public address, after construction.
// The daemon sets it from store.SettingPublicURL at load and again on reload,
// so a notification's deep link follows the address the operator changed on
// the system page.
func (n *Ntfy) SetPublicURL(u string) { n.publicURL = strings.TrimRight(strings.TrimSpace(u), "/") }

// LoadNtfy builds the transport from the configured ntfy values: the server URL
// and topic (ordinary settings, with a sealed fallback), plus the optional
// sealed access token. An absent URL or topic means the operator has not
// configured push at all, which is not an error: the caller gets a nil transport
// and a daemon that runs without one.
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
		return nil, fmt.Errorf("notify: %s is set but %s is not", SecretNtfyURL, SecretNtfyTopic)
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
	Click    string       `json:"click,omitempty"`
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
	title, body, buttons := n.render(note)
	click := n.resolveClick(note.Click)
	msg := ntfyMessage{
		Topic:    n.topic,
		Title:    title,
		Message:  body,
		Priority: int(note.Priority),
		Tags:     note.Tags,
		Click:    click,
	}
	// The action buttons carry a single-use capability URL, so they are gated
	// by the detail rung: a private operator (summary or terse) gets a
	// tap-to-open notification with no Approve/Reject on the lock screen.
	if buttons {
		for _, a := range note.Actions {
			msg.Actions = append(msg.Actions, ntfyAction{
				Action: "view", Label: a.Label, URL: a.URL, Clear: true,
			})
		}
	}

	encoded, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notify: encode ntfy message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base.String(), bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("notify: ntfy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	// X-Click and X-Tags duplicate the body's click/tags as the CLI-friendly
	// header form; the click is already rooted at the public URL, and the tags
	// carry only non-sensitive identifiers, so unlike Title/Body they are not
	// gated by Detail — a terse operator still gets them.
	if click != "" {
		req.Header.Set("X-Click", click)
	}
	if len(note.Tags) > 0 {
		req.Header.Set("X-Tags", strings.Join(note.Tags, ","))
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

// resolveClick roots a notification's deep link at the deployment's public
// address. The builders emit an exocomp:// resource URI (the same namespace the
// MCP resources use), which no browser and no stock notification client can
// open — a tap would do nothing. With a public URL set the link becomes a real
// address, {publicURL}/approvals/42, that opens the app on the approval page;
// without one it is left as written. A link that is already absolute, or that
// webPath does not recognise, is never mangled.
func (n *Ntfy) resolveClick(click string) string {
	if click == "" || n.publicURL == "" {
		return click
	}
	path, ok := webPath(click)
	if !ok {
		return click
	}
	return n.publicURL + path
}

// webPath maps an exocomp deep link — the exocomp:// resource URI a notification
// builder emits — to the web path that opens the same thing in the app. The
// mapping is explicit because the two namespaces are not the same string: a
// workspace's drift link and a secret's link each live on a page that is not
// their own name, so a blind prefix swap would send a tap to a 404. It reports
// false for anything it does not recognise, leaving an unknown link alone.
func webPath(click string) (string, bool) {
	rest, ok := strings.CutPrefix(click, "exocomp://")
	if !ok {
		return "", false
	}
	seg := strings.Split(strings.Trim(rest, "/"), "/")
	switch seg[0] {
	case "approvals":
		if len(seg) == 2 {
			return "/approvals/" + seg[1], true
		}
	case "jobs":
		if len(seg) >= 2 { // jobs/{id} and jobs/{id}/log both open the job page
			return "/jobs/" + seg[1], true
		}
	case "workspaces":
		if len(seg) >= 2 { // workspaces/{name} and .../{name}/drift
			return "/workspaces/" + seg[1], true
		}
		return "/workspaces", true
	case "secrets":
		return "/secrets", true // metadata only; there is no per-secret page
	case "schedules":
		return "/schedules", true
	case "checks":
		return "/checks", true
	case "events":
		return "/events", true
	case "system":
		if len(seg) == 2 {
			return "/system/" + seg[1], true
		}
	}
	return "", false
}

// render resolves the title, body, and whether the action buttons ride, for the
// configured detail rung. DetailFull passes the notification's own title and
// body through — every builder in this codebase composes them as the
// non-sensitive summary the platform contract requires — and keeps the buttons.
// DetailSummary replaces both with a fixed product title and a one-line kind,
// and drops the buttons; DetailTerse replaces the body with the fixed generic
// string.
func (n *Ntfy) render(note Notification) (title, body string, buttons bool) {
	switch n.detail {
	case DetailTerse:
		return terseTitle, terseBody, false
	case DetailSummary:
		return terseTitle, summaryLine(note.Tags), false
	default:
		return note.Title, note.Body, true
	}
}

// summaryLine is the non-identifying one-liner the summary rung carries: what
// KIND of thing needs attention, read from the notification's own kind tag —
// never a resource, a workspace, or a host. An unrecognized kind still says the
// product needs attention, so the line is never empty.
func summaryLine(tags []string) string {
	for _, t := range tags {
		kind, ok := strings.CutPrefix(t, "kind:")
		if !ok {
			continue
		}
		switch kind {
		case "approval":
			return "an approval is waiting — open to decide"
		case "job":
			return "a job needs attention — open to see why"
		case "drift":
			return "a workspace drifted — open to review"
		case "deadman":
			return "the dead-man tripped — open to check backups"
		case "expiry":
			return "a decision expired — open to see"
		}
	}
	return "exocomp needs attention — open to see"
}
