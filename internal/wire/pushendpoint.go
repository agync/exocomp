package wire

import "exocomp.dev/exocomp/internal/store"

const (
	// KindPushEndpoint is the kind of one registered push endpoint.
	KindPushEndpoint = "notify.endpoint"
	// KindPushEndpointList is the kind of a push-endpoint listing.
	KindPushEndpointList = "notify.endpoint.list"
)

// PushEndpointView projects one registered UnifiedPush endpoint. It carries the
// device-supplied distributor URL, its label, and when it was registered —
// nothing that could be a secret (the endpoint URL is the client's own, and no
// token material is echoed).
type PushEndpointView struct {
	ID        int64  `json:"id"`
	Endpoint  string `json:"endpoint"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at"`
}

// NewPushEndpointView projects one endpoint.
func NewPushEndpointView(e store.PushEndpoint) PushEndpointView {
	return PushEndpointView{
		ID:        e.ID,
		Endpoint:  e.URL,
		Label:     e.Label,
		CreatedAt: rfc3339(e.CreatedAt),
	}
}

// NewPushEndpointList projects an endpoint listing. A nil/empty rows still
// yields a non-nil slice, so the envelope renders "data":[] rather than null.
func NewPushEndpointList(rows []store.PushEndpoint) []PushEndpointView {
	out := make([]PushEndpointView, len(rows))
	for i, e := range rows {
		out[i] = NewPushEndpointView(e)
	}
	return out
}
