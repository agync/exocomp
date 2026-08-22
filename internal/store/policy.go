package store

import (
	"context"
	"fmt"
	"time"
)

// Plan-policy actions. A policy's verdict is one of these when its expression
// matches a sealed plan: deny makes the plan unconsumable, require_approval
// routes the apply through the approval gate.
const (
	PolicyDeny            = "deny"
	PolicyRequireApproval = "require_approval"
)

// PlanPolicy is one row of the plan_policy table: a CEL expression over a
// workspace's plan-show JSON and the action a match takes. The expression is
// compiled and rejected at write time by the caller, so a stored row is one the
// engine could always evaluate.
type PlanPolicy struct {
	ID        int64
	Name      string
	Workspace string
	Expr      string
	Action    string
	Enabled   bool
	CreatedAt time.Time
}

const planPolicyColumns = `id, name, workspace, expr, action, enabled, created_at`

// CreatePlanPolicy inserts an enabled policy. Names are unique across
// workspaces so `policy rm <name>` and the verdict record can name one row.
func (s *Store) CreatePlanPolicy(ctx context.Context, p PlanPolicy) (PlanPolicy, error) {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	p.Enabled = true

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO plan_policy (name, workspace, expr, action, enabled, created_at)
		VALUES (?, ?, ?, ?, 1, ?)`,
		p.Name, p.Workspace, p.Expr, p.Action, p.CreatedAt.UTC().UnixMilli())
	if err != nil {
		return PlanPolicy{}, wrap("create plan policy", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return PlanPolicy{}, wrap("create plan policy", err)
	}
	p.ID = id
	return p, nil
}

// ListPlanPolicies returns policies ordered by name. A non-empty workspace
// narrows to that workspace; enabledOnly narrows to the ones the seal-time
// engine evaluates.
func (s *Store) ListPlanPolicies(ctx context.Context, workspace string, enabledOnly bool) ([]PlanPolicy, error) {
	query := `SELECT ` + planPolicyColumns + ` FROM plan_policy`
	var (
		conds []string
		args  []any
	)
	if workspace != "" {
		conds = append(conds, "workspace = ?")
		args = append(args, workspace)
	}
	if enabledOnly {
		conds = append(conds, "enabled = 1")
	}
	for i, c := range conds {
		if i == 0 {
			query += " WHERE " + c
		} else {
			query += " AND " + c
		}
	}
	query += " ORDER BY name"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list plan policies", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PlanPolicy
	for rows.Next() {
		p, err := scanPlanPolicy(rows)
		if err != nil {
			return nil, wrap("list plan policies", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list plan policies", err)
	}
	return out, nil
}

// GetPlanPolicy returns the policy with the given name.
func (s *Store) GetPlanPolicy(ctx context.Context, name string) (PlanPolicy, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+planPolicyColumns+` FROM plan_policy WHERE name = ?`, name)
	p, err := scanPlanPolicy(row)
	if err != nil {
		return PlanPolicy{}, wrap("get plan policy", err)
	}
	return p, nil
}

// DeletePlanPolicy removes a policy by name.
func (s *Store) DeletePlanPolicy(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM plan_policy WHERE name = ?`, name)
	if err != nil {
		return wrap("delete plan policy", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete plan policy", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete plan policy %q: %w", name, ErrNotFound)
	}
	return nil
}

func scanPlanPolicy(row rowScanner) (PlanPolicy, error) {
	var (
		p         PlanPolicy
		enabled   int
		createdAt int64
	)
	err := row.Scan(&p.ID, &p.Name, &p.Workspace, &p.Expr, &p.Action, &enabled, &createdAt)
	if err != nil {
		return PlanPolicy{}, err
	}
	p.Enabled = enabled != 0
	p.CreatedAt = time.UnixMilli(createdAt).UTC()
	return p, nil
}
