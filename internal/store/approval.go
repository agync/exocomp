package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ApprovalState is where a decision request stands.
type ApprovalState string

// The approval states. Everything but pending is terminal, which is what
// makes a decision link single-use: the second tap finds a decided row.
const (
	ApprovalPending   ApprovalState = "pending"
	ApprovalApproved  ApprovalState = "approved"
	ApprovalRejected  ApprovalState = "rejected"
	ApprovalExpired   ApprovalState = "expired"
	ApprovalWithdrawn ApprovalState = "withdrawn"
)

// ErrApprovalDecided reports that an approval is no longer pending, so the
// decision presented arrived after the question was settled.
var ErrApprovalDecided = errors.New("store: approval already decided")

// Approval is a job waiting on a human. The job it names sits in
// awaiting_approval and no worker may claim it until this row says approved.
type Approval struct {
	ID           int64
	JobID        int64
	PlanArtifact *int64
	// Workspace and PlanSerial are the stale-plan check's inputs: the serial
	// the workspace stood at when the plan was made. A plan whose workspace
	// moved since is refused, never quietly applied.
	Workspace   string
	PlanSerial  int64
	RequestedAt time.Time
	ExpiresAt   time.Time
	// TokenHash is sha256 of the capability handed out. The token itself is
	// never stored, so a copy of the database approves nothing.
	TokenHash  string
	State      ApprovalState
	DecidedAt  *time.Time
	DecidedVia string
	Reason     string
}

const approvalColumns = `id, job_id, plan_artifact, workspace, plan_serial, requested_at,
	expires_at, token_hash, state, decided_at, decided_via, reason`

// CreateApproval inserts a pending approval for a job.
func (s *Store) CreateApproval(ctx context.Context, a Approval) (Approval, error) {
	if a.RequestedAt.IsZero() {
		a.RequestedAt = time.Now().UTC()
	}
	if a.ExpiresAt.IsZero() {
		return Approval{}, errors.New("store: create approval: an expiry is mandatory")
	}
	if a.TokenHash == "" {
		return Approval{}, errors.New("store: create approval: no token hash")
	}
	a.State = ApprovalPending

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO approval (job_id, plan_artifact, workspace, plan_serial, requested_at,
		                      expires_at, token_hash, state, decided_via, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '')`,
		a.JobID, nullInt64(a.PlanArtifact), a.Workspace, a.PlanSerial,
		a.RequestedAt.UTC().UnixMilli(), a.ExpiresAt.UTC().UnixMilli(),
		a.TokenHash, string(a.State))
	if err != nil {
		return Approval{}, wrap("create approval", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Approval{}, wrap("create approval", err)
	}
	a.ID = id
	return a, nil
}

// GetApproval returns the approval with the given id.
func (s *Store) GetApproval(ctx context.Context, id int64) (Approval, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approval WHERE id = ?`, id)
	a, err := scanApproval(row)
	if err != nil {
		return Approval{}, wrap("get approval", err)
	}
	return a, nil
}

// ApprovalByJob returns the approval gating a job, or ErrNotFound.
func (s *Store) ApprovalByJob(ctx context.Context, jobID int64) (Approval, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+approvalColumns+` FROM approval WHERE job_id = ?`, jobID)
	a, err := scanApproval(row)
	if err != nil {
		return Approval{}, wrap("approval by job", err)
	}
	return a, nil
}

// ListApprovals returns approvals newest first; pendingOnly narrows to the
// ones still waiting on a human.
func (s *Store) ListApprovals(ctx context.Context, pendingOnly bool) ([]Approval, error) {
	query := `SELECT ` + approvalColumns + ` FROM approval`
	var args []any
	if pendingOnly {
		query += ` WHERE state = ?`
		args = append(args, string(ApprovalPending))
	}
	query += ` ORDER BY requested_at DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list approvals", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, wrap("list approvals", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list approvals", err)
	}
	return out, nil
}

// DecideApproval moves a pending approval to a terminal state. The move is
// conditioned on the row still being pending in the same statement, so two
// taps of one link cannot both win: the loser gets ErrApprovalDecided and the
// caller answers with a terminal notice rather than an error.
func (s *Store) DecideApproval(ctx context.Context, id int64, to ApprovalState, via, reason string) (Approval, error) {
	if to == ApprovalPending {
		return Approval{}, fmt.Errorf("store: decide approval %d: %q is not a decision", id, to)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE approval SET state = ?, decided_at = ?, decided_via = ?, reason = ?
		WHERE id = ? AND state = ?`,
		string(to), time.Now().UTC().UnixMilli(), via, reason, id, string(ApprovalPending))
	if err != nil {
		return Approval{}, wrap("decide approval", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Approval{}, wrap("decide approval", err)
	}
	if affected == 1 {
		return s.GetApproval(ctx, id)
	}

	current, err := s.GetApproval(ctx, id)
	if err != nil {
		return Approval{}, err
	}
	return current, fmt.Errorf("store: approval %d is %s: %w", id, current.State, ErrApprovalDecided)
}

// PendingApprovalsExpiredAt returns the pending approvals whose TTL has run
// out by at. Expiry is fail-closed: the caller rejects each one's job.
func (s *Store) PendingApprovalsExpiredAt(ctx context.Context, at time.Time) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approval
		WHERE state = ? AND expires_at <= ? ORDER BY id`,
		string(ApprovalPending), at.UTC().UnixMilli())
	if err != nil {
		return nil, wrap("expired approvals", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, wrap("expired approvals", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("expired approvals", err)
	}
	return out, nil
}

func scanApproval(row rowScanner) (Approval, error) {
	var (
		a                       Approval
		planArtifact            sql.NullInt64
		requestedAt, expiresAt  int64
		decidedAt               sql.NullInt64
		state                   string
		planSerial, jobIDColumn int64
	)
	err := row.Scan(&a.ID, &jobIDColumn, &planArtifact, &a.Workspace, &planSerial,
		&requestedAt, &expiresAt, &a.TokenHash, &state, &decidedAt, &a.DecidedVia, &a.Reason)
	if err != nil {
		return Approval{}, err
	}
	a.JobID = jobIDColumn
	a.PlanArtifact = int64Ptr(planArtifact)
	a.PlanSerial = planSerial
	a.RequestedAt = time.UnixMilli(requestedAt).UTC()
	a.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	a.State = ApprovalState(state)
	a.DecidedAt = timePtr(decidedAt)
	return a, nil
}
