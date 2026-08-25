package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// UnifiedPush delivers a notification to every registered per-device push
// endpoint. Unlike ntfy/goalert it is not configured from a sealed secret: its
// targets are rows a device registers over the API, so it is store-backed and
// enumerates them on each Publish. No endpoints is a clean no-op.
//
// The endpoint URL is CLIENT-SUPPLIED, so the client this transport dials is
// wrapped with a net.Dialer.Control hook (dialControl) that refuses any socket
// whose resolved address is loopback/private/link-local/metadata — the send-time
// half of the two-layer SSRF guard, and the one that survives DNS rebinding.
type UnifiedPush struct {
	reg    PushRegistry
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
}

// PushRegistry is the store surface the transport reads and prunes. It is an
// interface so a test can drive the transport without a database.
type PushRegistry interface {
	ActivePushEndpoints(ctx context.Context) ([]store.PushEndpoint, error)
	MarkPushEndpointSuccess(ctx context.Context, id int64, at time.Time) error
	MarkPushEndpointError(ctx context.Context, id int64, msg string) error
	DeletePushEndpoint(ctx context.Context, id int64) error
}

const (
	// pushTimeout bounds one endpoint's POST. A phone whose distributor is slow
	// must not hold a guardrail decision open, exactly as for the other transports.
	pushTimeout = 10 * time.Second
	// pushMaxRespBody caps how much of a distributor's response is read back. The
	// body is drained and discarded; only the status code matters.
	pushMaxRespBody = 4 << 10
	// pushMaxError caps a stored last_error, so a hostile distributor cannot bloat
	// a row with a megabyte of "error".
	pushMaxError = 256
)

// NewUnifiedPush builds the transport over a registry. The HTTP client it dials
// through carries the dial-time SSRF guard; a caller that passes its own client
// (a test) is trusted to have set an equivalent Control hook.
func NewUnifiedPush(reg PushRegistry, log *slog.Logger) *UnifiedPush {
	if log == nil {
		log = slog.Default()
	}
	return &UnifiedPush{
		reg:    reg,
		client: guardedClient(),
		log:    log,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// guardedClient is an HTTP client whose dialer refuses to connect to a private,
// loopback, link-local, or metadata address — checked on the concrete IP the
// socket is about to reach, after DNS.
func guardedClient() *http.Client {
	dialer := &net.Dialer{Timeout: pushTimeout, Control: dialControl}
	return &http.Client{
		Timeout: pushTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   pushTimeout,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// Name implements Transport.
func (u *UnifiedPush) Name() string { return "unifiedpush" }

// pushMessage is the document a distributor relays to the phone. It reuses the
// Notification fields the ntfy X-Click/X-Tags work already exposes, so a
// subscriber decodes the same shape whichever transport carried it.
type pushMessage struct {
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message"`
	Priority int      `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Click    string   `json:"click,omitempty"`
}

// Publish implements Transport by fanning n out to every registered endpoint.
// Per-endpoint failures are isolated: one endpoint being down, refused at dial
// time, or gone never blocks the others, and never fails the Publish for the
// other transports — a bad endpoint's error is logged and recorded on its row,
// not returned. A 404/410 means the distributor dropped the subscription, so the
// endpoint is self-pruned. No endpoints is a clean nil.
func (u *UnifiedPush) Publish(ctx context.Context, n Notification) error {
	endpoints, err := u.reg.ActivePushEndpoints(ctx)
	if err != nil {
		return fmt.Errorf("notify: push: list endpoints: %w", err)
	}
	if len(endpoints) == 0 {
		return nil
	}

	body, err := json.Marshal(pushMessage{
		Title:    n.Title,
		Message:  n.Body,
		Priority: int(n.Priority),
		Tags:     n.Tags,
		Click:    n.Click,
	})
	if err != nil {
		return fmt.Errorf("notify: push: encode message: %w", err)
	}

	for _, e := range endpoints {
		u.deliver(ctx, e, body)
	}
	return nil
}

// deliver posts one message to one endpoint and records the outcome on its row.
// It never returns: an endpoint's fate is its own, isolated from its siblings.
func (u *UnifiedPush) deliver(ctx context.Context, e store.PushEndpoint, body []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		u.fail(ctx, e, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := u.client.Do(req)
	if err != nil {
		// A dial refused by the SSRF Control hook surfaces here as a *url.Error;
		// it is recorded and logged like any other delivery failure, never fatal.
		u.fail(ctx, e, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, pushMaxRespBody))

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// The distributor dropped this subscription; stop pushing to it.
		if derr := u.reg.DeletePushEndpoint(ctx, e.ID); derr != nil {
			u.log.Error("notify: push: prune dropped endpoint", "endpoint", e.ID, "err", derr)
		} else {
			u.log.Info("notify: push: pruned dropped endpoint", "endpoint", e.ID, "status", resp.StatusCode)
		}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		u.fail(ctx, e, fmt.Errorf("distributor answered %s", resp.Status))
	default:
		if merr := u.reg.MarkPushEndpointSuccess(ctx, e.ID, u.now()); merr != nil {
			u.log.Error("notify: push: record success", "endpoint", e.ID, "err", merr)
		}
	}
}

// fail logs and records one endpoint's delivery failure, truncating the stored
// reason so a hostile distributor cannot bloat the row.
func (u *UnifiedPush) fail(ctx context.Context, e store.PushEndpoint, err error) {
	u.log.Warn("notify: push: delivery failed", "endpoint", e.ID, "err", err)
	msg := err.Error()
	if len(msg) > pushMaxError {
		msg = msg[:pushMaxError]
	}
	if merr := u.reg.MarkPushEndpointError(ctx, e.ID, msg); merr != nil {
		u.log.Error("notify: push: record error", "endpoint", e.ID, "err", merr)
	}
}
