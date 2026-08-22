package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// CreateGatedJob inserts a job directly into awaiting_approval, the state no
// worker claims. It is one INSERT rather than a create-then-transition
// because a job that exists as queued for even an instant is claimable, and a
// destructive action must never be runnable before the human has answered.
func (s *Store) CreateGatedJob(ctx context.Context, n NewJob) (Job, error) {
	args, err := json.Marshal(n.Args)
	if err != nil {
		return Job{}, fmt.Errorf("store: create gated job: encode args: %w", err)
	}
	now := time.Now().UTC()

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO job (runbook, kind, "trigger", state, args_json, queued_at,
		                 concurrency_key, dedup_key, idempotency_key, owner_pid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.Runbook, n.Kind, n.Trigger, string(StateAwaitingApproval), string(args),
		now.UnixMilli(), n.ConcurrencyKey, nullStr(n.DedupKey),
		nullStr(n.IdempotencyKey), os.Getpid())
	if err != nil {
		return Job{}, wrap("create gated job", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Job{}, wrap("create gated job", err)
	}
	return s.GetJob(ctx, id)
}

// PendingApprovalsByDedupKey returns the pending approvals whose job carries
// dedupKey. It is how an alert that resolves finds the approval it raised:
// the job and the event share the key the source deduplicates on.
func (s *Store) PendingApprovalsByDedupKey(ctx context.Context, dedupKey string) ([]Approval, error) {
	if dedupKey == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+prefixed("a", approvalColumns)+` FROM approval AS a
		JOIN job AS j ON j.id = a.job_id
		WHERE a.state = ? AND j.dedup_key = ? ORDER BY a.id`,
		string(ApprovalPending), dedupKey)
	if err != nil {
		return nil, wrap("pending approvals by dedup key", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, wrap("pending approvals by dedup key", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("pending approvals by dedup key", err)
	}
	return out, nil
}

// prefixed qualifies a column list with a table alias, so a join can reuse the
// same list the single-table queries do.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
