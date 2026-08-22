package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Rule action kinds. A rule's action_args_json is read according to the kind:
// shell carries argv, the tofu kinds carry a workspace name, notify carries
// nothing and produces no job at all.
const (
	ActionShell     = "shell"
	ActionTofuPlan  = "tofu-plan"
	ActionTofuApply = "tofu-apply"
	ActionNotify    = "notify"
)

// RuleAction is a rule's action_args_json.
type RuleAction struct {
	Argv      []string `json:"argv,omitempty"`
	Workspace string   `json:"workspace,omitempty"`
	Timeout   Duration `json:"timeout,omitempty"`
}

// Rule is one row of the rule table: a CEL expression over a normalized event
// and the action a match runs, with the guardrails that bound it.
type Rule struct {
	ID              int64
	Name            string
	SourceFilter    string
	CELExpr         string
	ActionKind      string
	Action          RuleAction
	CooldownS       int
	DailyCap        int
	RequireApproval bool
	Enabled         bool
}

// NewRule describes a rule to create. It starts enabled.
type NewRule struct {
	Name            string
	SourceFilter    string
	CELExpr         string
	ActionKind      string
	Action          RuleAction
	CooldownS       int
	DailyCap        int
	RequireApproval bool
}

const ruleColumns = `id, name, source_filter, cel_expr, action_kind,
	action_args_json, cooldown_s, daily_cap, require_approval, enabled`

// CreateRule inserts an enabled rule. Names are unique.
func (s *Store) CreateRule(ctx context.Context, n NewRule) (Rule, error) {
	args, err := json.Marshal(n.Action)
	if err != nil {
		return Rule{}, fmt.Errorf("store: create rule: encode action: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO rule (name, source_filter, cel_expr, action_kind, action_args_json,
		                  cooldown_s, daily_cap, require_approval, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		n.Name, n.SourceFilter, n.CELExpr, n.ActionKind, string(args),
		n.CooldownS, n.DailyCap, n.RequireApproval)
	if err != nil {
		return Rule{}, wrap("create rule", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Rule{}, wrap("create rule", err)
	}
	return s.GetRuleByID(ctx, id)
}

// GetRuleByID returns the rule with the given id.
func (s *Store) GetRuleByID(ctx context.Context, id int64) (Rule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM rule WHERE id = ?`, id)
	r, err := scanRule(row)
	if err != nil {
		return Rule{}, wrap("get rule", err)
	}
	return r, nil
}

// GetRule returns the rule with the given name.
func (s *Store) GetRule(ctx context.Context, name string) (Rule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM rule WHERE name = ?`, name)
	r, err := scanRule(row)
	if err != nil {
		return Rule{}, wrap("get rule", err)
	}
	return r, nil
}

// ListRules returns rules ordered by name; enabledOnly narrows to the rules
// the engine evaluates.
func (s *Store) ListRules(ctx context.Context, enabledOnly bool) ([]Rule, error) {
	query := `SELECT ` + ruleColumns + ` FROM rule`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY name`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, wrap("list rules", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, wrap("list rules", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list rules", err)
	}
	return out, nil
}

// SetRuleEnabled flips a rule's enabled flag by name — the manual
// `rule enable`/`rule disable` path. A daily cap no longer calls this: the cap
// throttles a rule (withholds the action, keeps it armed) rather than disabling
// it, so a flood cannot blind a security rule (finding R3).
func (s *Store) SetRuleEnabled(ctx context.Context, name string, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE rule SET enabled = ? WHERE name = ?`, enabled, name)
	if err != nil {
		return wrap("set rule enabled", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set rule enabled", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set rule enabled %q: %w", name, ErrNotFound)
	}
	return nil
}

// DeleteRule removes a rule by name, and with it its fire ledger.
func (s *Store) DeleteRule(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM rule WHERE name = ?`, name)
	if err != nil {
		return wrap("delete rule", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete rule", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete rule %q: %w", name, ErrNotFound)
	}
	return nil
}

// RuleFire is one entry of a rule's fire ledger.
type RuleFire struct {
	ID     int64
	RuleID int64
	Ts     time.Time
	JobID  *int64
}

// RecordRuleFire appends to the ledger the cooldown and the daily cap are read
// from. It is written when a rule's action reaches the queue — gated or
// running — because a gated action has spent the rule's budget just as much as
// one that ran.
func (s *Store) RecordRuleFire(ctx context.Context, ruleID int64, jobID *int64) (RuleFire, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO rule_fire (rule_id, ts, job_id) VALUES (?, ?, ?)`,
		ruleID, now.UnixMilli(), nullInt64(jobID))
	if err != nil {
		return RuleFire{}, wrap("record rule fire", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return RuleFire{}, wrap("record rule fire", err)
	}
	return RuleFire{ID: id, RuleID: ruleID, Ts: now, JobID: jobID}, nil
}

// LastRuleFire returns the newest fire of a rule, or ErrNotFound when it has
// never fired.
func (s *Store) LastRuleFire(ctx context.Context, ruleID int64) (RuleFire, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, rule_id, ts, job_id FROM rule_fire
		WHERE rule_id = ? ORDER BY ts DESC, id DESC LIMIT 1`, ruleID)
	f, err := scanRuleFire(row)
	if err != nil {
		return RuleFire{}, wrap("last rule fire", err)
	}
	return f, nil
}

// CountRuleFires counts a rule's fires at or after since.
func (s *Store) CountRuleFires(ctx context.Context, ruleID int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rule_fire WHERE rule_id = ? AND ts >= ?`,
		ruleID, since.UTC().UnixMilli()).Scan(&n)
	if err != nil {
		return 0, wrap("count rule fires", err)
	}
	return n, nil
}

func scanRule(row rowScanner) (Rule, error) {
	var (
		r                        Rule
		argsJSON                 string
		requireApproval, enabled int
		cooldownS, dailyCap      int64
	)
	err := row.Scan(&r.ID, &r.Name, &r.SourceFilter, &r.CELExpr, &r.ActionKind,
		&argsJSON, &cooldownS, &dailyCap, &requireApproval, &enabled)
	if err != nil {
		return Rule{}, err
	}
	r.CooldownS = int(cooldownS)
	r.DailyCap = int(dailyCap)
	r.RequireApproval = requireApproval != 0
	r.Enabled = enabled != 0
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &r.Action); err != nil {
			return Rule{}, fmt.Errorf("decode action of rule %d: %w", r.ID, err)
		}
	}
	return r, nil
}

func scanRuleFire(row rowScanner) (RuleFire, error) {
	var (
		f     RuleFire
		ts    int64
		jobID sql.NullInt64
	)
	if err := row.Scan(&f.ID, &f.RuleID, &ts, &jobID); err != nil {
		return RuleFire{}, err
	}
	f.Ts = time.UnixMilli(ts).UTC()
	f.JobID = int64Ptr(jobID)
	return f, nil
}
