// Package notify delivers the messages a human has to act on: an approval
// waiting, a rule disabled by its own cap, a decision that expired.
//
// One interface, so a second transport is a constructor rather than a rewrite.
// Delivery is never the arbiter of anything: a transport that fails is logged
// and the decision it carried still stands in the database.
package notify

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Priority ranks a notification. The ladder is ntfy's, because it is the one
// the phone's notification channels already understand.
type Priority int

// The priorities. Max is reserved for what only a human can unblock — an
// approval about to expire, or a rule throttled at its daily cap paging once.
const (
	PriorityMin     Priority = 1
	PriorityLow     Priority = 2
	PriorityDefault Priority = 3
	PriorityHigh    Priority = 4
	PriorityMax     Priority = 5
)

// Action is a tappable button. The URL is a single-use capability, so it names
// no host, workspace, or resource: the lock screen shows a count and a TTL,
// and what it wants to do is behind the tap (ui-brief, approval surface
// contract 1).
type Action struct {
	Label string
	URL   string
}

// Notification is one message.
type Notification struct {
	Title    string
	Body     string
	Priority Priority
	// Tags are structured, non-sensitive extras a subscriber renders from
	// instead of parsing a capability URL: an id, a ttl in seconds, a kind —
	// alongside any ntfy emoji shortcode a transport also reads them as (e.g.
	// "warning"). Never a secret, a plan, or the decision token.
	Tags []string
	// Click is the deep link a subscriber opens on tap of the notification
	// itself, e.g. "exocomp://approvals/42" or "exocomp://jobs/7". Like Tags
	// it names only a non-sensitive identifier — never the capability action
	// URL an Action carries, which is a single-use bearer token.
	Click   string
	Actions []Action
	// Source and Host attribute the notification so a scoped mute can hold it.
	// They are optional: an unattributed notification is held only by a global
	// mute, never by an alert- or host-scoped one.
	Source string
	Host   string
	// Resolved marks a notification as the clearing of a prior condition rather
	// than a fresh one. A transport that models incidents (GoAlert) closes the
	// matching alert instead of opening one; a fire-and-forget transport (ntfy)
	// treats it as any other message. It defaults to false, so every existing
	// caller keeps opening.
	Resolved bool
}

// Transport delivers notifications somewhere.
type Transport interface {
	// Name identifies the transport in logs and in doctor output.
	Name() string
	// Publish delivers one notification, or reports why it could not.
	Publish(ctx context.Context, n Notification) error
}

// Notifier fans a notification out to every configured transport.
type Notifier struct {
	// mu guards the transport set, which a daemon reload replaces while a
	// publish may be in flight. A publish copies the slice under the read lock
	// and then uses its own copy, so a swap never races it.
	mu         sync.RWMutex
	transports []Transport
	mutes      MuteChecker
	log        *slog.Logger
}

// New returns a Notifier over the given transports. No transports is a valid
// configuration: an operator who has not set one up gets a working daemon and
// a silent phone, not a daemon that refuses to start.
func New(log *slog.Logger, transports ...Transport) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{transports: liveTransports(transports), log: log}
}

// liveTransports drops the nil transports a caller may pass — an unconfigured
// transport is a nil, and a nil transport is not a publish target.
func liveTransports(transports []Transport) []Transport {
	live := make([]Transport, 0, len(transports))
	for _, t := range transports {
		if t != nil {
			live = append(live, t)
		}
	}
	return live
}

// SetTransports replaces the fan-out's transports, so a daemon reload can pick
// up a changed ntfy configuration, a sealed goalert endpoint, a rotated
// credential, or a cleared configuration without a restart. It returns how many
// transports are now live. A publish already in flight keeps the slice it read,
// so the swap never races one.
func (n *Notifier) SetTransports(transports ...Transport) int {
	live := liveTransports(transports)
	n.mu.Lock()
	n.transports = live
	n.mu.Unlock()
	return len(live)
}

// WithMutes attaches a mute checker. A held notification is not published,
// logged as muted with the reason, and reported by Notify as delivered (nil):
// a mute is a deliberate quiet, not a failure, so nothing downstream retries
// or escalates it.
func (n *Notifier) WithMutes(m MuteChecker) *Notifier {
	if n != nil {
		n.mu.Lock()
		n.mutes = m
		n.mu.Unlock()
	}
	return n
}

// Transports reports how many transports are configured.
func (n *Notifier) Transports() int {
	if n == nil {
		return 0
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.transports)
}

// Notify publishes to every transport, returning the failures joined. Every
// transport is tried even when an earlier one fails: they exist so that one
// being down is survivable.
func (n *Notifier) Notify(ctx context.Context, note Notification) error {
	if n == nil {
		return nil
	}
	n.mu.RLock()
	transports := n.transports
	mutes := n.mutes
	n.mu.RUnlock()
	if len(transports) == 0 {
		return nil
	}
	if mutes != nil {
		if mute, held := mutes.Held(ctx, note.Source, note.Host); held {
			n.log.Info("notify: held by mute",
				"scope", mute.Scope, "value", mute.Value,
				"reason", mute.Reason, "until", mute.ExpiresAt.Format(time.RFC3339))
			return nil
		}
	}
	var errs []error
	for _, t := range transports {
		if err := t.Publish(ctx, note); err != nil {
			n.log.Error("notify: publish failed", "transport", t.Name(), "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
