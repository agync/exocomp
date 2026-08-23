package wire

import "exocomp.dev/exocomp/internal/store"

// KindMuteList is the kind of a mute listing.
const KindMuteList = "mute.list"

// MuteView projects one notification mute: its scope, why it was set, how it
// was created, and when it lifts. CreatedAt is omitted — ExpiresAt is the one
// timestamp an agent branches on (still active vs. lifted).
type MuteView struct {
	ID         int64  `json:"id"`
	ScopeKind  string `json:"scope_kind"`
	ScopeValue string `json:"scope_value,omitempty"`
	Reason     string `json:"reason,omitempty"`
	CreatedVia string `json:"created_via,omitempty"`
	ExpiresAt  string `json:"expires_at"`
}

// NewMuteList projects a mute listing. A nil/empty rows still yields a
// non-nil slice, so the envelope renders "data":[] rather than null.
func NewMuteList(rows []store.NotifyMute) []MuteView {
	out := make([]MuteView, len(rows))
	for i, m := range rows {
		out[i] = MuteView{
			ID:         m.ID,
			ScopeKind:  m.ScopeKind,
			ScopeValue: m.ScopeValue,
			Reason:     m.Reason,
			CreatedVia: m.CreatedVia,
			ExpiresAt:  rfc3339(m.ExpiresAt),
		}
	}
	return out
}
