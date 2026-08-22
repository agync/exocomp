package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StepStatus is a workflow step's position in its small state machine:
// pending -> running -> succeeded|failed, plus blocked for a step a prior
// failure retired before it ran.
type StepStatus string

// The step statuses. succeeded, failed and blocked are terminal.
const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	StepBlocked   StepStatus = "blocked"
)

// WorkflowState is a whole workflow's derived status, read from its steps
// rather than stored: the ledger's rows are the source of truth, so there is no
// second place to keep consistent.
type WorkflowState string

// The workflow states.
const (
	WorkflowRunning   WorkflowState = "running"
	WorkflowSucceeded WorkflowState = "succeeded"
	WorkflowFailed    WorkflowState = "failed"
)

// WorkflowStep is one row of the step ledger.
type WorkflowStep struct {
	WorkflowID     string
	StepNo         int
	Kind           string
	Status         StepStatus
	IdempotencyKey string
	JobID          *int64
	ResultJSON     string
	RollbackMarker *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewWorkflowStep describes a step to create. Every step starts pending.
type NewWorkflowStep struct {
	StepNo         int
	Kind           string
	IdempotencyKey string
	JobID          *int64
}

const workflowStepColumns = `workflow_id, step_no, kind, status, idempotency_key,
	job_id, result_json, rollback_marker, created_at, updated_at`

// CreateWorkflow inserts a workflow's steps, all pending, in one transaction:
// a half-written ledger would let a resume run a workflow whose later steps it
// cannot see. It rejects an empty step list and a workflow_id that already has
// rows (ErrDuplicate via the primary key).
func (s *Store) CreateWorkflow(ctx context.Context, workflowID string, steps []NewWorkflowStep) error {
	if workflowID == "" {
		return fmt.Errorf("store: create workflow: empty workflow id")
	}
	if len(steps) == 0 {
		return fmt.Errorf("store: create workflow: no steps")
	}

	now := time.Now().UTC().UnixMilli()
	return s.InTx(ctx, func(tx *sql.Tx) error {
		for _, step := range steps {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO workflow_step (workflow_id, step_no, kind, status,
				    idempotency_key, job_id, result_json, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)`,
				workflowID, step.StepNo, step.Kind, string(StepPending),
				step.IdempotencyKey, jobIDArg(step.JobID), now, now)
			if err != nil {
				return wrap("create workflow", err)
			}
		}
		return nil
	})
}

// GetWorkflow returns a workflow's steps in step order, or ErrNotFound when the
// id names no workflow.
func (s *Store) GetWorkflow(ctx context.Context, workflowID string) ([]WorkflowStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_step
		 WHERE workflow_id = ? ORDER BY step_no`, workflowID)
	if err != nil {
		return nil, wrap("get workflow", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkflowStep
	for rows.Next() {
		step, err := scanWorkflowStep(rows)
		if err != nil {
			return nil, wrap("get workflow", err)
		}
		out = append(out, step)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("get workflow", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("store: get workflow %q: %w", workflowID, ErrNotFound)
	}
	return out, nil
}

// GetWorkflowStep returns one step, or ErrNotFound.
func (s *Store) GetWorkflowStep(ctx context.Context, workflowID string, stepNo int) (WorkflowStep, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+workflowStepColumns+` FROM workflow_step
		 WHERE workflow_id = ? AND step_no = ?`, workflowID, stepNo)
	step, err := scanWorkflowStep(row)
	if err != nil {
		return WorkflowStep{}, wrap("get workflow step", err)
	}
	return step, nil
}

// ListIncompleteWorkflows returns the ids of workflows with a step still to
// run, in id order. A workflow whose steps are all terminal — every step
// succeeded, or a failure blocked the rest — is complete and absent here, so a
// resume never re-drives a workflow that already reached its end. Backed by the
// partial index migration 0011 creates.
func (s *Store) ListIncompleteWorkflows(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT workflow_id FROM workflow_step
		WHERE status IN (?, ?) ORDER BY workflow_id`,
		string(StepPending), string(StepRunning))
	if err != nil {
		return nil, wrap("list incomplete workflows", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrap("list incomplete workflows", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list incomplete workflows", err)
	}
	return out, nil
}

// StartStep moves a pending step to running and records the job that will run
// it. The guard (WHERE status = pending) makes it atomic: two processes cannot
// both start the same step, and a step already past pending is left untouched.
func (s *Store) StartStep(ctx context.Context, workflowID string, stepNo int, jobID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_step SET status = ?, job_id = ?, updated_at = ?
		WHERE workflow_id = ? AND step_no = ? AND status = ?`,
		string(StepRunning), jobID, time.Now().UTC().UnixMilli(),
		workflowID, stepNo, string(StepPending))
	if err != nil {
		return wrap("start step", err)
	}
	return s.oneRow(ctx, res, workflowID, stepNo, StepPending)
}

// CompleteStepTx moves a running step to a terminal status inside the caller's
// transaction. This is the ledger's whole crash-safety contract (ADR 0008):
// the caller writes the step's own effect and this ledger row in one tx, so a
// crash cannot land the effect while leaving the step pending — the state a
// resume would re-run. status must be succeeded or failed.
//
// The guard (WHERE status = running) also makes a duplicate completion a
// conflict rather than a silent second write, which a reconcile after a crash
// relies on to tell "already done" from "still to do".
func (s *Store) CompleteStepTx(ctx context.Context, tx *sql.Tx, workflowID string, stepNo int, status StepStatus, result string) error {
	if status != StepSucceeded && status != StepFailed {
		return fmt.Errorf("store: complete step: status %q is not terminal", status)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_step SET status = ?, result_json = ?, updated_at = ?
		WHERE workflow_id = ? AND step_no = ? AND status = ?`,
		string(status), result, time.Now().UTC().UnixMilli(),
		workflowID, stepNo, string(StepRunning))
	if err != nil {
		return wrap("complete step", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("complete step", err)
	}
	if affected == 1 {
		return nil
	}
	// The read for the conflict message must go through the caller's tx: the
	// store holds a single connection, so a fresh query on s.db here would
	// deadlock against the transaction that connection is running.
	var cur string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM workflow_step WHERE workflow_id = ? AND step_no = ?`,
		workflowID, stepNo).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: complete step %s/%d: %w", workflowID, stepNo, ErrNotFound)
	}
	if err != nil {
		return wrap("complete step", err)
	}
	return fmt.Errorf("store: workflow %s step %d is %s, not %s: %w",
		workflowID, stepNo, cur, StepRunning, ErrStateConflict)
}

// CompleteStep is CompleteStepTx for a step whose effect is nothing but its own
// outcome — a subprocess step, whose durable record is its job row, not a store
// write to co-commit. It still runs in a transaction so the ledger write is
// atomic on its own.
func (s *Store) CompleteStep(ctx context.Context, workflowID string, stepNo int, status StepStatus, result string) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		return s.CompleteStepTx(ctx, tx, workflowID, stepNo, status, result)
	})
}

// BlockPendingSteps retires every still-pending step of a workflow to blocked,
// the terminal state for work a prior failure means will never run. It returns
// the number blocked.
func (s *Store) BlockPendingSteps(ctx context.Context, workflowID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_step SET status = ?, updated_at = ?
		WHERE workflow_id = ? AND status = ?`,
		string(StepBlocked), time.Now().UTC().UnixMilli(),
		workflowID, string(StepPending))
	if err != nil {
		return 0, wrap("block pending steps", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, wrap("block pending steps", err)
	}
	return int(affected), nil
}

// RetargetStep resets a running step back to pending under a fresh idempotency
// key, so a resume can re-run a step whose previous job died mid-flight without
// colliding with that job's now-terminal key. The guard keeps it to the
// running state a reconcile decided to retry.
func (s *Store) RetargetStep(ctx context.Context, workflowID string, stepNo int, newKey string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_step SET status = ?, idempotency_key = ?, job_id = NULL, updated_at = ?
		WHERE workflow_id = ? AND step_no = ? AND status = ?`,
		string(StepPending), newKey, time.Now().UTC().UnixMilli(),
		workflowID, stepNo, string(StepRunning))
	if err != nil {
		return wrap("retarget step", err)
	}
	return s.oneRow(ctx, res, workflowID, stepNo, StepRunning)
}

// SetRollbackMarker records a step's undo point for a rollback choreography to
// read later.
func (s *Store) SetRollbackMarker(ctx context.Context, workflowID string, stepNo int, marker string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_step SET rollback_marker = ?, updated_at = ?
		WHERE workflow_id = ? AND step_no = ?`,
		marker, time.Now().UTC().UnixMilli(), workflowID, stepNo)
	if err != nil {
		return wrap("set rollback marker", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set rollback marker", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set rollback marker %s/%d: %w", workflowID, stepNo, ErrNotFound)
	}
	return nil
}

// WorkflowStatus derives a workflow's state from its steps. A failure anywhere
// makes the workflow failed; otherwise all-succeeded is succeeded and anything
// unfinished is running.
func WorkflowStatus(steps []WorkflowStep) WorkflowState {
	allSucceeded := true
	for _, step := range steps {
		switch step.Status {
		case StepFailed, StepBlocked:
			return WorkflowFailed
		case StepSucceeded:
		default:
			allSucceeded = false
		}
	}
	if allSucceeded {
		return WorkflowSucceeded
	}
	return WorkflowRunning
}

// InTx runs fn inside one immediate write transaction, committing when it
// returns nil and rolling back otherwise. It is the seam the ledger's
// same-transaction contract needs: a caller writes its step's effect and the
// CompleteStepTx ledger row through the same tx, so the two are one atomic
// commit.
func (s *Store) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrap("begin tx", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return wrap("commit tx", err)
	}
	return nil
}

// oneRow turns a guarded single-row update into an error when it matched no
// row: either the step is gone (ErrNotFound) or it left the expected status
// first (ErrStateConflict).
func (s *Store) oneRow(ctx context.Context, res sql.Result, workflowID string, stepNo int, want StepStatus) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("workflow step update", err)
	}
	if affected == 1 {
		return nil
	}
	return s.stepConflict(ctx, workflowID, stepNo, want)
}

func (s *Store) stepConflict(ctx context.Context, workflowID string, stepNo int, want StepStatus) error {
	step, err := s.GetWorkflowStep(ctx, workflowID, stepNo)
	if err != nil {
		return err
	}
	return fmt.Errorf("store: workflow %s step %d is %s, not %s: %w",
		workflowID, stepNo, step.Status, want, ErrStateConflict)
}

func jobIDArg(id *int64) any {
	if id == nil {
		return nil
	}
	return *id
}

func scanWorkflowStep(row rowScanner) (WorkflowStep, error) {
	var (
		step      WorkflowStep
		status    string
		jobID     sql.NullInt64
		rollback  sql.NullString
		createdAt int64
		updatedAt int64
	)
	err := row.Scan(&step.WorkflowID, &step.StepNo, &step.Kind, &status,
		&step.IdempotencyKey, &jobID, &step.ResultJSON, &rollback,
		&createdAt, &updatedAt)
	if err != nil {
		return WorkflowStep{}, err
	}
	step.Status = StepStatus(status)
	if jobID.Valid {
		step.JobID = &jobID.Int64
	}
	if rollback.Valid {
		step.RollbackMarker = &rollback.String
	}
	step.CreatedAt = time.UnixMilli(createdAt).UTC()
	step.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return step, nil
}
