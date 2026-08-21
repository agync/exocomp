package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Event levels written to job_event.
const (
	LevelInfo   = "info"
	LevelError  = "error"
	LevelSystem = "system"
)

// JobEvent is one append-only line of a job's history.
type JobEvent struct {
	JobID   int64
	Seq     int64
	Ts      time.Time
	Level   string
	Msg     string
	RawJSON string
}

// AppendJobEvent appends an event, assigning the next sequence number for the
// job. job_event is append-only: there is no update or delete counterpart.
func (s *Store) AppendJobEvent(ctx context.Context, e JobEvent) (JobEvent, error) {
	if e.Level == "" {
		e.Level = LevelInfo
	}
	if e.Ts.IsZero() {
		e.Ts = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return JobEvent{}, wrap("append job event", err)
	}
	defer func() { _ = tx.Rollback() }()

	var next int64
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM job_event WHERE job_id = ?`, e.JobID).Scan(&next)
	if err != nil {
		return JobEvent{}, wrap("append job event", err)
	}
	e.Seq = next

	_, err = tx.ExecContext(ctx, `
		INSERT INTO job_event (job_id, seq, ts, level, msg, raw_json)
		VALUES (?, ?, ?, ?, ?, ?)`,
		e.JobID, e.Seq, e.Ts.UnixMilli(), e.Level, e.Msg, nullStr(e.RawJSON))
	if err != nil {
		return JobEvent{}, wrap("append job event", err)
	}
	if err := tx.Commit(); err != nil {
		return JobEvent{}, wrap("append job event", err)
	}
	return e, nil
}

// ListJobEvents returns a job's events in sequence order.
func (s *Store) ListJobEvents(ctx context.Context, jobID int64) ([]JobEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT job_id, seq, ts, level, msg, raw_json
		FROM job_event WHERE job_id = ? ORDER BY seq`, jobID)
	if err != nil {
		return nil, wrap("list job events", err)
	}
	return scanJobEvents(rows)
}

// LastJobEvents returns the final n events of a job in sequence order.
func (s *Store) LastJobEvents(ctx context.Context, jobID int64, n int) ([]JobEvent, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT job_id, seq, ts, level, msg, raw_json FROM (
			SELECT job_id, seq, ts, level, msg, raw_json
			FROM job_event WHERE job_id = ? ORDER BY seq DESC LIMIT ?
		) ORDER BY seq`, jobID, n)
	if err != nil {
		return nil, wrap("last job events", err)
	}
	return scanJobEvents(rows)
}

func scanJobEvents(rows *sql.Rows) ([]JobEvent, error) {
	defer func() { _ = rows.Close() }()

	var out []JobEvent
	for rows.Next() {
		var (
			e   JobEvent
			ts  int64
			raw sql.NullString
		)
		if err := rows.Scan(&e.JobID, &e.Seq, &ts, &e.Level, &e.Msg, &raw); err != nil {
			return nil, fmt.Errorf("store: scan job event: %w", err)
		}
		e.Ts = time.UnixMilli(ts).UTC()
		if raw.Valid {
			e.RawJSON = raw.String
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("scan job events", err)
	}
	return out, nil
}
