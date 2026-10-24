package client

import (
	"context"
	"net/url"
	"strconv"

	"exocomp.dev/exocomp/internal/wire"
)

// This file is the typed read surface: one method per /api/v1 GET route, each
// decoding into the SAME wire projection type the daemon marshalled and the
// local CLI's --output json emits. Re-emitting the returned value through
// wire.WriteSuccess therefore reproduces the daemon's bytes exactly, which is
// what keeps remote and local --output json byte-identical.

// JobFilter mirrors the query parameters GET /api/v1/jobs accepts (which mirror
// store.JobFilter). It is defined here so the client does not import store.
type JobFilter struct {
	States         []string
	Runbook        string
	ConcurrencyKey string
	Limit          int
}

func (f JobFilter) query() url.Values {
	q := url.Values{}
	for _, s := range f.States {
		q.Add("state", s)
	}
	if f.Runbook != "" {
		q.Set("runbook", f.Runbook)
	}
	if f.ConcurrencyKey != "" {
		q.Set("concurrency_key", f.ConcurrencyKey)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return q
}

// ListJobs returns jobs matching the filter, oldest-first as the store orders.
func (c *Client) ListJobs(ctx context.Context, f JobFilter) ([]wire.JobView, error) {
	var out []wire.JobView
	if err := c.get(ctx, "/api/v1/jobs", f.query(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetJob returns one job; a well-formed but unknown id yields an APIError with
// CodeNotFound.
func (c *Client) GetJob(ctx context.Context, id int64) (wire.JobView, error) {
	var out wire.JobView
	err := c.get(ctx, "/api/v1/jobs/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}

// JobEvents returns a job's append-only, already-redacted event log. since>0
// returns only events after that sequence, so an agent tails without re-reading.
func (c *Client) JobEvents(ctx context.Context, id int64, since int64) ([]wire.JobEventView, error) {
	q := url.Values{}
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	var out []wire.JobEventView
	if err := c.get(ctx, "/api/v1/jobs/"+strconv.FormatInt(id, 10)+"/events", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListWorkspaces returns registered workspaces in store order.
func (c *Client) ListWorkspaces(ctx context.Context) ([]wire.WorkspaceView, error) {
	var out []wire.WorkspaceView
	if err := c.get(ctx, "/api/v1/workspaces", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListSources returns registered sources in store order.
func (c *Client) ListSources(ctx context.Context) ([]wire.SourceView, error) {
	var out []wire.SourceView
	if err := c.get(ctx, "/api/v1/sources", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListApprovals returns approvals; all=false shows pending-only (the default),
// matching `approval list`.
func (c *Client) ListApprovals(ctx context.Context, all bool) ([]wire.ApprovalView, error) {
	q := url.Values{}
	if all {
		q.Set("all", "true")
	}
	var out []wire.ApprovalView
	if err := c.get(ctx, "/api/v1/approvals", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetApproval returns one approval by id.
func (c *Client) GetApproval(ctx context.Context, id int64) (wire.ApprovalView, error) {
	var out wire.ApprovalView
	err := c.get(ctx, "/api/v1/approvals/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}

// ListSchedules returns schedules in store order.
func (c *Client) ListSchedules(ctx context.Context) ([]wire.ScheduleView, error) {
	var out []wire.ScheduleView
	if err := c.get(ctx, "/api/v1/schedules", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListRules returns reactive rules in store order.
func (c *Client) ListRules(ctx context.Context) ([]wire.RuleView, error) {
	var out []wire.RuleView
	if err := c.get(ctx, "/api/v1/rules", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListSilences returns silences; all=false shows active-only (the default).
func (c *Client) ListSilences(ctx context.Context, all bool) ([]wire.SilenceView, error) {
	q := url.Values{}
	if all {
		q.Set("all", "true")
	}
	var out []wire.SilenceView
	if err := c.get(ctx, "/api/v1/silences", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EventFilter mirrors the query parameters GET /api/v1/events accepts.
type EventFilter struct {
	Source  string
	Outcome string
	Limit   int
	// All asks for every event, which the route reads from an explicit limit of 0;
	// without it a zero Limit leaves the daemon's default of 25.
	All bool
}

func (f EventFilter) query() url.Values {
	q := url.Values{}
	if f.Source != "" {
		q.Set("source", f.Source)
	}
	if f.Outcome != "" {
		q.Set("outcome", f.Outcome)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	} else if f.All {
		q.Set("limit", "0")
	}
	return q
}

// ListEvents returns received events, newest-first.
func (c *Client) ListEvents(ctx context.Context, f EventFilter) ([]wire.EventView, error) {
	var out []wire.EventView
	if err := c.get(ctx, "/api/v1/events", f.query(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetEvent returns one received event by id.
func (c *Client) GetEvent(ctx context.Context, id int64) (wire.EventView, error) {
	var out wire.EventView
	err := c.get(ctx, "/api/v1/events/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}

// ListSecrets returns secret METADATA only — the values-free SecretView has no
// field a sealed value could occupy.
func (c *Client) ListSecrets(ctx context.Context) ([]wire.SecretView, error) {
	var out []wire.SecretView
	if err := c.get(ctx, "/api/v1/secrets", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListBackups returns backup targets in store order.
func (c *Client) ListBackups(ctx context.Context) ([]wire.BackupTargetView, error) {
	var out []wire.BackupTargetView
	if err := c.get(ctx, "/api/v1/backups", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Health returns the daemon's doctor summary: one status per subsystem, the same
// probe `doctor --output json` runs.
func (c *Client) Health(ctx context.Context) (wire.HealthView, error) {
	var out wire.HealthView
	if err := c.get(ctx, "/api/v1/health", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
