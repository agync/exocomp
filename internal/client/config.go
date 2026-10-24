package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"exocomp.dev/exocomp/internal/wire"
)

// This file is the typed surface of the silence, mute, check and netgroup
// routes (internal/web/api_write.go). Lists come back through ListSilences,
// ListMutes and the wire views; ListEvents lives with the reads.

// Response kinds of the config routes. The create and update routes answer with
// the kinds the daemon names. The removals answer 204 with no envelope, so the
// Lifted and Removed kinds below are the client's own, used when a remote
// removal is re-emitted under --output json.
const (
	KindSilence         = "silence.get"
	KindMute            = "mute.get"
	KindSilenceLifted   = "silence.lifted"
	KindMuteLifted      = "mute.lifted"
	KindCheckRemoved    = "check.removed"
	KindNetgroupRemoved = "netgroup.removed"
)

// HoldRequest is `silence add` and `mute add` as fields. Scope is alert, host or
// global, and For is a positive Go duration.
type HoldRequest struct {
	ScopeKind  string `json:"scope_kind"`
	ScopeValue string `json:"scope_value,omitempty"`
	Reason     string `json:"reason,omitempty"`
	For        string `json:"for"`
}

// CheckRequest is `check add` and `check update` as fields. Targets are the
// name=spec strings the flags take. The kind fields are pointers so an omitted
// one differs from a zero value, as the daemon requires. An update ignores Name
// and Kind, which the stored check fixes, and keeps its interval when Interval
// is empty.
type CheckRequest struct {
	Name       string   `json:"name,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Interval   string   `json:"interval,omitempty"`
	Targets    []string `json:"targets"`
	Expect     *int     `json:"expect,omitempty"`
	MinDays    *int     `json:"min_days,omitempty"`
	ExpectAddr *string  `json:"expect_addr,omitempty"`
	Op         *string  `json:"op,omitempty"`
	Value      *float64 `json:"value,omitempty"`
}

// callNoContent sends a request whose success is a 204.
func (c *Client) callNoContent(ctx context.Context, method, path string) error {
	kind, _, err := c.do(ctx, method, path, nil, nil, "")
	if err != nil {
		return err
	}
	if kind != "" {
		return fmt.Errorf("client: %s %s answered with unexpected kind %q", method, path, kind)
	}
	return nil
}

// CreateSilence silences a scope on the daemon until the deadline.
func (c *Client) CreateSilence(ctx context.Context, req HoldRequest) (wire.SilenceView, error) {
	var out wire.SilenceView
	err := c.call(ctx, http.MethodPost, "/api/v1/silences", nil, req, KindSilence, &out)
	return out, err
}

// RemoveSilence lifts a silence before it expires.
func (c *Client) RemoveSilence(ctx context.Context, id int64) error {
	return c.callNoContent(ctx, http.MethodDelete, "/api/v1/silences/"+strconv.FormatInt(id, 10))
}

// CreateMute holds notifications for a scope on the daemon until the deadline.
func (c *Client) CreateMute(ctx context.Context, req HoldRequest) (wire.MuteView, error) {
	var out wire.MuteView
	err := c.call(ctx, http.MethodPost, "/api/v1/mutes", nil, req, KindMute, &out)
	return out, err
}

// ListMutes returns mutes; all=false shows active-only (the default).
func (c *Client) ListMutes(ctx context.Context, all bool) ([]wire.MuteView, error) {
	q := url.Values{}
	if all {
		q.Set("all", "true")
	}
	var out []wire.MuteView
	if err := c.get(ctx, "/api/v1/mutes", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RemoveMute lifts a mute before it expires.
func (c *Client) RemoveMute(ctx context.Context, id int64) error {
	return c.callNoContent(ctx, http.MethodDelete, "/api/v1/mutes/"+strconv.FormatInt(id, 10))
}

// CreateCheck registers a check on the daemon.
func (c *Client) CreateCheck(ctx context.Context, req CheckRequest) (wire.CheckView, error) {
	var out wire.CheckView
	err := c.call(ctx, http.MethodPost, "/api/v1/checks", nil, req, wire.KindCheck, &out)
	return out, err
}

// UpdateCheck re-points a stored check, keeping the state of kept targets.
func (c *Client) UpdateCheck(ctx context.Context, name string, req CheckRequest) (wire.CheckView, error) {
	var out wire.CheckView
	err := c.call(ctx, http.MethodPut, "/api/v1/checks/"+pathEscape(name), nil, req, wire.KindCheck, &out)
	return out, err
}

// SetCheckEnabled enables or disables a check.
func (c *Client) SetCheckEnabled(ctx context.Context, name string, enabled bool) (wire.CheckView, error) {
	var out wire.CheckView
	err := c.call(ctx, http.MethodPost, "/api/v1/checks/"+pathEscape(name)+"/"+enableVerb(enabled), nil, nil,
		wire.KindCheck, &out)
	return out, err
}

// RemoveCheck deletes a check and its state.
func (c *Client) RemoveCheck(ctx context.Context, name string) error {
	return c.callNoContent(ctx, http.MethodDelete, "/api/v1/checks/"+pathEscape(name))
}

// ProbeCheck runs a stored check's probes on the daemon now, writing nothing.
// target narrows it to one target when not empty.
func (c *Client) ProbeCheck(ctx context.Context, name, target string) (wire.CheckProbeView, error) {
	var q url.Values
	if target != "" {
		q = url.Values{"target": {target}}
	}
	var out wire.CheckProbeView
	err := c.call(ctx, http.MethodPost, "/api/v1/checks/"+pathEscape(name)+"/probe", q, nil, wire.KindCheckProbe, &out)
	return out, err
}

// ListNetgroups returns the named CIDR sets in store order.
func (c *Client) ListNetgroups(ctx context.Context) ([]wire.NetgroupView, error) {
	var out []wire.NetgroupView
	if err := c.get(ctx, "/api/v1/netgroups", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateNetgroup registers a named CIDR set; prefixes is the comma-separated
// list the CLI takes, with a `!` marking an exclusion.
func (c *Client) CreateNetgroup(ctx context.Context, name, prefixes string) (wire.NetgroupView, error) {
	var out wire.NetgroupView
	err := c.call(ctx, http.MethodPost, "/api/v1/netgroups", nil,
		map[string]string{"name": name, "prefixes": prefixes}, wire.KindNetgroup, &out)
	return out, err
}

// UpdateNetgroup replaces a group's set in place.
func (c *Client) UpdateNetgroup(ctx context.Context, name, prefixes string) (wire.NetgroupView, error) {
	var out wire.NetgroupView
	err := c.call(ctx, http.MethodPut, "/api/v1/netgroups/"+pathEscape(name), nil,
		map[string]string{"prefixes": prefixes}, wire.KindNetgroup, &out)
	return out, err
}

// RemoveNetgroup deletes a named CIDR set.
func (c *Client) RemoveNetgroup(ctx context.Context, name string) error {
	return c.callNoContent(ctx, http.MethodDelete, "/api/v1/netgroups/"+pathEscape(name))
}
