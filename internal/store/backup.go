package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Backup run states.
const (
	BackupRunning   = "running"
	BackupSucceeded = "succeeded"
	BackupFailed    = "failed"
)

// BackupTarget is one leg of the backup fan-out.
type BackupTarget struct {
	ID        int64
	Name      string
	Kind      string // restic_local|restic_s3|restic_sftp|git
	URI       string
	Retention string
	Enabled   bool
}

// BackupRun is one attempt against one target. A failing leg never aborts the
// others, so each carries its own state and error.
type BackupRun struct {
	ID         int64
	TargetID   int64
	StartedAt  time.Time
	FinishedAt *time.Time
	State      string
	SnapshotID string
	BytesAdded int64
	Error      string
}

// CreateBackupTarget adds a target. Names are unique.
func (s *Store) CreateBackupTarget(ctx context.Context, t BackupTarget) (BackupTarget, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO backup_target (name, kind, uri, retention, enabled) VALUES (?, ?, ?, ?, ?)`,
		t.Name, t.Kind, t.URI, t.Retention, t.Enabled)
	if err != nil {
		return BackupTarget{}, wrap("create backup target", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return BackupTarget{}, wrap("create backup target", err)
	}
	t.ID = id
	return t, nil
}

// GetBackupTarget returns a target by name.
func (s *Store) GetBackupTarget(ctx context.Context, name string) (BackupTarget, error) {
	var t BackupTarget
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, kind, uri, retention, enabled FROM backup_target WHERE name = ?`, name).
		Scan(&t.ID, &t.Name, &t.Kind, &t.URI, &t.Retention, &t.Enabled)
	if err != nil {
		return BackupTarget{}, wrap("get backup target", err)
	}
	return t, nil
}

// ListBackupTargets returns targets; disabled ones only when includeDisabled
// is set.
func (s *Store) ListBackupTargets(ctx context.Context, includeDisabled bool) ([]BackupTarget, error) {
	query := `SELECT id, name, kind, uri, retention, enabled FROM backup_target`
	if !includeDisabled {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, wrap("list backup targets", err)
	}
	defer func() { _ = rows.Close() }()

	var out []BackupTarget
	for rows.Next() {
		var t BackupTarget
		if err := rows.Scan(&t.ID, &t.Name, &t.Kind, &t.URI, &t.Retention, &t.Enabled); err != nil {
			return nil, fmt.Errorf("store: scan backup target: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list backup targets", err)
	}
	return out, nil
}

// UpdateBackupTarget rewrites a target's mutable fields.
func (s *Store) UpdateBackupTarget(ctx context.Context, t BackupTarget) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE backup_target SET kind = ?, uri = ?, retention = ?, enabled = ? WHERE id = ?`,
		t.Kind, t.URI, t.Retention, t.Enabled, t.ID)
	if err != nil {
		return wrap("update backup target", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("update backup target", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: update backup target %d: %w", t.ID, ErrNotFound)
	}
	return nil
}

// DeleteBackupTarget removes a target and, by cascade, its run history.
func (s *Store) DeleteBackupTarget(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM backup_target WHERE id = ?`, id)
	if err != nil {
		return wrap("delete backup target", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete backup target", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete backup target %d: %w", id, ErrNotFound)
	}
	return nil
}

// StartBackupRun opens a running row for a target.
func (s *Store) StartBackupRun(ctx context.Context, targetID int64) (BackupRun, error) {
	run := BackupRun{TargetID: targetID, StartedAt: time.Now().UTC(), State: BackupRunning}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO backup_run (target_id, started_at, state) VALUES (?, ?, ?)`,
		run.TargetID, run.StartedAt.UnixMilli(), run.State)
	if err != nil {
		return BackupRun{}, wrap("start backup run", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return BackupRun{}, wrap("start backup run", err)
	}
	run.ID = id
	return run, nil
}

// FinishBackupRun closes a run with its outcome.
func (s *Store) FinishBackupRun(ctx context.Context, run BackupRun) error {
	finished := time.Now().UTC()
	if run.FinishedAt != nil {
		finished = *run.FinishedAt
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE backup_run
		SET finished_at = ?, state = ?, snapshot_id = ?, bytes_added = ?, error = ?
		WHERE id = ?`,
		finished.UnixMilli(), run.State, run.SnapshotID, run.BytesAdded, run.Error, run.ID)
	if err != nil {
		return wrap("finish backup run", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("finish backup run", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: finish backup run %d: %w", run.ID, ErrNotFound)
	}
	return nil
}

// ListBackupRuns returns a target's runs, newest first, capped at limit when
// limit is positive.
func (s *Store) ListBackupRuns(ctx context.Context, targetID int64, limit int) ([]BackupRun, error) {
	query := `SELECT id, target_id, started_at, finished_at, state, snapshot_id, bytes_added, error
	          FROM backup_run WHERE target_id = ? ORDER BY started_at DESC, id DESC`
	args := []any{targetID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list backup runs", err)
	}
	defer func() { _ = rows.Close() }()

	var out []BackupRun
	for rows.Next() {
		var (
			run        BackupRun
			startedAt  int64
			finishedAt sql.NullInt64
		)
		err := rows.Scan(&run.ID, &run.TargetID, &startedAt, &finishedAt, &run.State,
			&run.SnapshotID, &run.BytesAdded, &run.Error)
		if err != nil {
			return nil, fmt.Errorf("store: scan backup run: %w", err)
		}
		run.StartedAt = time.UnixMilli(startedAt).UTC()
		run.FinishedAt = timePtr(finishedAt)
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list backup runs", err)
	}
	return out, nil
}
