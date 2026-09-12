package web

import (
	"context"
	"errors"
)

// NtfyDriver is the daemon's ntfy state and test-push seam (SPEC §2). The web
// surface holds no open envelope, so the ntfy leg's judgment — the NewNtfy
// refusal rule (internal/notify/ntfy.go) — and its push path live behind this
// interface the daemon provides, the same shape as Maintenance. A nil driver
// (a surface built without one) degrades the leg to presence-only and answers
// the Test verb with "not available on this surface".
type NtfyDriver interface {
	// State reports the ntfy leg's judgment: which sealed keys exist and
	// whether the configured channel was refused at load. It re-derives the
	// NewNtfy rule from the sealed secrets the daemon holds — presence, never
	// values.
	State(ctx context.Context) (NtfySeamState, error)
	// TestPush sends one test notification through the live transport. A
	// channel that cannot load (the NewNtfy refusal) returns an error wrapped
	// in ErrNtfyRefused; a delivery failure returns the transport's own error.
	TestPush(ctx context.Context, host string) error
}

// NtfySeamState is the ntfy leg's public face: the sealed keys' presence and
// the server class — a fact about the URL, never the URL itself.
type NtfySeamState struct {
	URLSealed   bool
	TopicSealed bool
	TokenSealed bool
	// ServerClass is "public" | "self-hosted", "" when no URL is sealed.
	ServerClass string
	// Refused is the NewNtfy judgment: a public topic without an access token.
	Refused bool
}

// ErrNtfyRefused marks the Test verb's refusal case — the channel cannot load
// because the NewNtfy rule rejected it (a public topic without a token, or a
// half-configured one). The drawer renders the binding copy line for it; any
// other TestPush error is a delivery failure.
var ErrNtfyRefused = errors.New("ntfy: refused at load")

// NtfyState is the leg's three-word judgment rendered on the card, the
// drawer, and the powerline: ON / REFUSED / OFF.
type NtfyState string

// The three legal judgments.
const (
	NtfyStateOff     NtfyState = "off"
	NtfyStateOn      NtfyState = "on"
	NtfyStateRefused NtfyState = "refused"
)
