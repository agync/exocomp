package reactive

import (
	"context"
	"errors"
	"net/netip"

	"exocomp.dev/exocomp/internal/store"
)

// stampPeer labels an event with the network path it arrived on: the peer's IP
// and the netgroup that most specifically contains it, or "away" when no
// registered group does. It is a network-path signal (LAN vs overlay), not a
// location verdict — over tailnet the peer is the CGNAT 100.64.0.0/10 address,
// so a home netgroup of LAN prefixes marks every overlay-delivered hook away,
// which is the fail-safe direction for a "refuse from outside" rule. The value
// comes from where the connection physically arrived, so the sender cannot
// forge it.
//
// A failed lookup is logged and swallowed, never returned: a webhook must not
// be refused because the netgroup table was unreadable, and an unstamped event
// is one no netgroup rule matches — the same safe no-match as "away".
func (e *Engine) stampPeer(ctx context.Context, ev *Event, addr netip.Addr) {
	if ev == nil {
		return
	}
	if ev.Labels == nil {
		ev.Labels = make(map[string]string)
	}
	addr = addr.Unmap()
	ev.Labels["peer_ip"] = addr.String()

	group, err := e.store.NetgroupForAddr(ctx, addr)
	switch {
	case errors.Is(err, store.ErrNotFound):
		ev.Labels["peer_netgroup"] = "away"
	case err != nil:
		e.log.Error("reactive: netgroup lookup for peer", "peer_ip", addr.String(), "err", err)
		ev.Labels["peer_netgroup"] = "away"
	default:
		ev.Labels["peer_netgroup"] = group
	}
}
