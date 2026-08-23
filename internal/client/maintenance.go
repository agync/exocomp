package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

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
