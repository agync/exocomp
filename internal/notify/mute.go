package notify

import (
	"context"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// Mute is an active hold on notifications: a scope, a mandatory expiry, and the
// reason it was set. It is the MUTED half of the banner — the job the held
// notification would have announced still ran.
type Mute struct {
	Scope     string
	Value     string
	Reason    string
	ExpiresAt time.Time
}

// MuteChecker decides whether a notification is currently held. It is a
// separate concern from the transports: a muted notification is not a delivery
// failure, it is a deliberate quiet, so the decision it carried still stands
// in the database whether or not the phone ever lit up.
type MuteChecker interface {
	// Held reports the mute covering a notification from the given alert name
	// and host, if any. The bool is false when nothing holds it.
	Held(ctx context.Context, alert, host string) (Mute, bool)
}

// StoreMutes is a MuteChecker backed by the notify_mute table. A read failure
// is treated as "not muted": a database hiccup must not silence the phone, it
// must let the notification through and be visible in the logs.
type StoreMutes struct {
	store *store.Store
}

// NewStoreMutes returns a MuteChecker reading active mutes from the store.
func NewStoreMutes(st *store.Store) *StoreMutes { return &StoreMutes{store: st} }

// Held returns the first active mute that covers the notification.
func (s *StoreMutes) Held(ctx context.Context, alert, host string) (Mute, bool) {
	if s == nil || s.store == nil {
		return Mute{}, false
	}
	mutes, err := s.store.ListNotifyMutes(ctx, true)
	if err != nil {
		return Mute{}, false
	}
	for _, m := range mutes {
		if m.Matches(alert, host) {
			return Mute{
				Scope:     m.ScopeKind,
				Value:     m.ScopeValue,
				Reason:    m.Reason,
				ExpiresAt: m.ExpiresAt,
			}, true
		}
	}
	return Mute{}, false
}
