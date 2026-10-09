package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"exocomp.dev/exocomp/internal/wire"
)

// This file is the typed drain-control surface: cordon a running daemon for a
// planned reboot, poll whether it is safe yet, and list the jobs a reboot left
// interrupted. Cordon/uncordon are operator-scope writes; readiness and the
// interrupted listing are reads. Every method decodes into the SAME wire
// projection the daemon marshals, so a remote `--output json` is byte-identical
// to a local one.

// Cordon puts the remote daemon into maintenance mode — no new schedule fires,
// no new job claims — and returns the resulting readiness.
func (c *Client) Cordon(ctx context.Context) (wire.MaintenanceView, error) {
	return c.maintenanceWrite(ctx, "/api/v1/maintenance/cordon")
}

// Uncordon lifts maintenance mode, re-enabling firing and claiming.
func (c *Client) Uncordon(ctx context.Context) (wire.MaintenanceView, error) {
	return c.maintenanceWrite(ctx, "/api/v1/maintenance/uncordon")
}

// Reload rebuilds the remote daemon's schedule registrations from the database
// — the same path SIGHUP and the reload interval drive — and returns how many
// enabled schedules the daemon now holds.
func (c *Client) Reload(ctx context.Context) (wire.ReloadView, error) {
	kind, data, err := c.do(ctx, http.MethodPost, "/api/v1/maintenance/reload", nil, nil, "")
	if err != nil {
		return wire.ReloadView{}, err
	}
	if kind != wire.KindMaintenanceReload {
		return wire.ReloadView{}, fmt.Errorf("client: maintenance reload answered with unexpected kind %q", kind)
	}
	var out wire.ReloadView
	if err := json.Unmarshal(data, &out); err != nil {
		return wire.ReloadView{}, fmt.Errorf("client: decode %s payload: %w", kind, err)
	}
	return out, nil
}

// Restart asks the remote daemon to restart after draining. The request is a
// one-way signal — the daemon cordons and answers at once, then drains and
// re-execs in the background — so the returned view is the pending state, not
// the outcome. force skips the wait and cancels the remaining jobs.
func (c *Client) Restart(ctx context.Context, force bool) (wire.RestartView, error) {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	kind, data, err := c.do(ctx, http.MethodPost, "/api/v1/maintenance/restart", q, nil, "")
	if err != nil {
		return wire.RestartView{}, err
	}
	if kind != wire.KindMaintenanceRestart {
		return wire.RestartView{}, fmt.Errorf("client: maintenance restart answered with unexpected kind %q", kind)
	}
	var out wire.RestartView
	if err := json.Unmarshal(data, &out); err != nil {
		return wire.RestartView{}, fmt.Errorf("client: decode %s payload: %w", kind, err)
	}
	return out, nil
}

// RestartStatus reports a pending restart's drain progress. A `restart --wait`
// polls it until the daemon goes down (the restart fired) or the pending flag
// clears (the drain timed out and the cordon was lifted).
func (c *Client) RestartStatus(ctx context.Context) (wire.RestartView, error) {
	var out wire.RestartView
	err := c.get(ctx, "/api/v1/maintenance/restart", nil, &out)
	return out, err
}

func (c *Client) maintenanceWrite(ctx context.Context, path string) (wire.MaintenanceView, error) {
	kind, data, err := c.do(ctx, http.MethodPost, path, nil, nil, "")
	if err != nil {
		return wire.MaintenanceView{}, err
	}
	if kind != wire.KindMaintenance {
		return wire.MaintenanceView{}, fmt.Errorf("client: maintenance answered with unexpected kind %q", kind)
	}
	var out wire.MaintenanceView
	if err := json.Unmarshal(data, &out); err != nil {
		return wire.MaintenanceView{}, fmt.Errorf("client: decode %s payload: %w", kind, err)
	}
	return out, nil
}

// Readiness reports whether the remote daemon is safe to reboot: cordoned and
// with nothing still in flight.
func (c *Client) Readiness(ctx context.Context) (wire.MaintenanceView, error) {
	var out wire.MaintenanceView
	err := c.get(ctx, "/api/v1/maintenance/readiness", nil, &out)
	return out, err
}

// ListInterrupted returns the jobs a reboot (or crash) left interrupted, newest
// first — the post-reboot view an operator re-drives from.
func (c *Client) ListInterrupted(ctx context.Context) ([]wire.JobView, error) {
	var out []wire.JobView
	if err := c.get(ctx, "/api/v1/maintenance/interrupted", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
