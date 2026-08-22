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
)

// Priority ranks a notification. The ladder is ntfy's, because it is the one
// the phone's notification channels already understand.
type Priority int

// The priorities. Max is reserved for what only a human can unblock — a rule
// its cap disabled, an approval about to expire.
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
	Tags     []string
	Actions  []Action
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
	transports []Transport
	log        *slog.Logger
}

// New returns a Notifier over the given transports. No transports is a valid
// configuration: an operator who has not set one up gets a working daemon and
// a silent phone, not a daemon that refuses to start.
func New(log *slog.Logger, transports ...Transport) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	live := make([]Transport, 0, len(transports))
	for _, t := range transports {
		if t != nil {
			live = append(live, t)
		}
	}
	return &Notifier{transports: live, log: log}
}

// Transports reports how many transports are configured.
func (n *Notifier) Transports() int { return len(n.transports) }

// Notify publishes to every transport, returning the failures joined. Every
// transport is tried even when an earlier one fails: they exist so that one
// being down is survivable.
func (n *Notifier) Notify(ctx context.Context, note Notification) error {
	if n == nil || len(n.transports) == 0 {
		return nil
	}
	var errs []error
	for _, t := range n.transports {
		if err := t.Publish(ctx, note); err != nil {
			n.log.Error("notify: publish failed", "transport", t.Name(), "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
