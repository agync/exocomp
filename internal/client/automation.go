package client

import (
	"context"
	"net/http"

	"exocomp.dev/exocomp/internal/wire"
)

// This file is the typed surface of the schedule and rule writes
// (internal/web/api_automation.go). Lists are read through ListSchedules and
// ListRules.

// Schedule and rule response kinds, mirroring internal/web/api_automation.go.
const (
	KindSchedule        = "schedule.get"
	KindScheduleRemoved = "schedule.removed"
	KindRule            = "rule.get"
	KindRuleRemoved     = "rule.removed"
)

// ScheduleRequest is `schedule add` as fields; the daemon checks it the way its
// scheduler will read it.
type ScheduleRequest struct {
	Name                string   `json:"name"`
	Cron                string   `json:"cron"`
	Kind                string   `json:"kind"`
	Argv                []string `json:"argv,omitempty"`
	Target              string   `json:"target,omitempty"`
	Source              string   `json:"source,omitempty"`
	TolerateUnreachable bool     `json:"tolerate_unreachable,omitempty"`
	ConcurrencyKey      string   `json:"concurrency_key,omitempty"`
	JitterS             int      `json:"jitter_s,omitempty"`
}

// RuleRequest is `rule add` as fields. Source is the playbook action's
// registered source; SourceFilter is the event-source filter.
type RuleRequest struct {
	Name             string   `json:"name"`
	SourceFilter     string   `json:"source_filter,omitempty"`
	Expr             string   `json:"expr"`
	Action           string   `json:"action"`
	Argv             []string `json:"argv,omitempty"`
	Workspace        string   `json:"workspace,omitempty"`
	Source           string   `json:"source,omitempty"`
	Playbook         string   `json:"playbook,omitempty"`
	Limit            string   `json:"limit,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Check            bool     `json:"check,omitempty"`
	Diff             bool     `json:"diff,omitempty"`
	Timeout          string   `json:"timeout,omitempty"`
	Egress           []int    `json:"egress,omitempty"`
	SSHIdentityFiles []string `json:"ssh_identity_files,omitempty"`
	CooldownS        int      `json:"cooldown_s,omitempty"`
	DailyCap         int      `json:"daily_cap,omitempty"`
	RequireApproval  bool     `json:"require_approval,omitempty"`
}

// Removed names a removed schedule or rule.
type Removed struct {
	Name string `json:"name"`
}

// CreateSchedule registers a schedule on the daemon.
func (c *Client) CreateSchedule(ctx context.Context, req ScheduleRequest) (wire.ScheduleView, error) {
	var out wire.ScheduleView
	err := c.call(ctx, http.MethodPost, "/api/v1/schedules", nil, req, KindSchedule, &out)
	return out, err
}

// RemoveSchedule removes a schedule.
func (c *Client) RemoveSchedule(ctx context.Context, name string) (Removed, error) {
	var out Removed
	err := c.call(ctx, http.MethodDelete, "/api/v1/schedules/"+pathEscape(name), nil, nil, KindScheduleRemoved, &out)
	return out, err
}

// SetScheduleEnabled enables or disables a schedule.
func (c *Client) SetScheduleEnabled(ctx context.Context, name string, enabled bool) (wire.ScheduleView, error) {
	var out wire.ScheduleView
	err := c.call(ctx, http.MethodPost, "/api/v1/schedules/"+pathEscape(name)+"/"+enableVerb(enabled), nil, nil,
		KindSchedule, &out)
	return out, err
}

// CreateRule registers a rule on the daemon.
func (c *Client) CreateRule(ctx context.Context, req RuleRequest) (wire.RuleView, error) {
	var out wire.RuleView
	err := c.call(ctx, http.MethodPost, "/api/v1/rules", nil, req, KindRule, &out)
	return out, err
}

// RemoveRule removes a rule and its fire ledger.
func (c *Client) RemoveRule(ctx context.Context, name string) (Removed, error) {
	var out Removed
	err := c.call(ctx, http.MethodDelete, "/api/v1/rules/"+pathEscape(name), nil, nil, KindRuleRemoved, &out)
	return out, err
}

// SetRuleEnabled enables or disables a rule.
func (c *Client) SetRuleEnabled(ctx context.Context, name string, enabled bool) (wire.RuleView, error) {
	var out wire.RuleView
	err := c.call(ctx, http.MethodPost, "/api/v1/rules/"+pathEscape(name)+"/"+enableVerb(enabled), nil, nil,
		KindRule, &out)
	return out, err
}

func enableVerb(enabled bool) string {
	if enabled {
		return "enable"
	}
	return "disable"
}
