package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Schedule kinds. The scheduler package interprets Args by kind: shell needs
// Argv, backup uses Target (empty means every enabled target).
const (
	ScheduleShell  = "shell"
	ScheduleBackup = "backup"
)

// ScheduleArgs is what a fire needs, shaped by the schedule's kind.
type ScheduleArgs struct {
	Argv   []string `json:"argv,omitempty"`
	Target string   `json:"target,omitempty"`
	// TolerateUnreachable, on a kind-play schedule, runs each fire under the
	// unreachable-tolerant outcome: a converge whose only non-success is offline
	// hosts settles degraded, not failed, so a scheduled play against a fleet
	// with a roaming machine does not page every time that machine sleeps.
	TolerateUnreachable bool `json:"tolerate_unreachable,omitempty"`
}

// Schedule is one row of the schedule table.
type Schedule struct {
	ID             int64
	Name           string
	CronExpr       string
	Kind           string
	Args           ScheduleArgs
	ConcurrencyKey string
	JitterS        int
	Enabled        bool
	LastJobID      *int64
	NextAt         *time.Time
}

// NewSchedule describes a schedule to create.
type NewSchedule struct {
	Name           string
	CronExpr       string
	Kind           string
	Args           ScheduleArgs
	ConcurrencyKey string
	JitterS        int
}

const scheduleColumns = `id, name, cron_expr, kind, args_json, concurrency_key,
	jitter_s, enabled, last_job_id, next_at`

// CreateSchedule inserts an enabled schedule. Names are unique.
func (s *Store) CreateSchedule(ctx context.Context, n NewSchedule) (Schedule, error) {
	args, err := json.Marshal(n.Args)
	if err != nil {
		return Schedule{}, fmt.Errorf("store: create schedule: encode args: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO schedule (name, cron_expr, kind, args_json, concurrency_key, jitter_s, enabled)
		VALUES (?, ?, ?, ?, ?, ?, 1)`,
		n.Name, n.CronExpr, n.Kind, string(args), n.ConcurrencyKey, n.JitterS)
	if err != nil {
		return Schedule{}, wrap("create schedule", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Schedule{}, wrap("create schedule", err)
	}
	return s.GetScheduleByID(ctx, id)
}

// GetScheduleByID returns the schedule with the given id.
func (s *Store) GetScheduleByID(ctx context.Context, id int64) (Schedule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedule WHERE id = ?`, id)
	sc, err := scanSchedule(row)
	if err != nil {
		return Schedule{}, wrap("get schedule", err)
	}
	return sc, nil
}

// GetSchedule returns the schedule with the given name.
func (s *Store) GetSchedule(ctx context.Context, name string) (Schedule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedule WHERE name = ?`, name)
	sc, err := scanSchedule(row)
	if err != nil {
		return Schedule{}, wrap("get schedule", err)
	}
	return sc, nil
}

// ListSchedules returns schedules ordered by name; enabledOnly narrows to
// schedules the scheduler should register.
func (s *Store) ListSchedules(ctx context.Context, enabledOnly bool) ([]Schedule, error) {
	query := `SELECT ` + scheduleColumns + ` FROM schedule`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY name`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, wrap("list schedules", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, wrap("list schedules", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list schedules", err)
	}
	return out, nil
}

// SetScheduleEnabled flips a schedule's enabled flag by name. A disabled
// schedule stays in the table (and its history) but ListSchedules(true) and
// so Scheduler.Reload skip it.
func (s *Store) SetScheduleEnabled(ctx context.Context, name string, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE schedule SET enabled = ? WHERE name = ?`, enabled, name)
	if err != nil {
		return wrap("set schedule enabled", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set schedule enabled", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set schedule enabled %q: %w", name, ErrNotFound)
	}
	return nil
}

// DeleteSchedule removes a schedule by name.
func (s *Store) DeleteSchedule(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM schedule WHERE name = ?`, name)
	if err != nil {
		return wrap("delete schedule", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete schedule", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete schedule %q: %w", name, ErrNotFound)
	}
	return nil
}

// RecordScheduleFire stamps the outcome of one cron fire. lastJobID is nil
// when the fire enqueued nothing (e.g. a backup schedule with no enabled
// targets); nextAt is nil when the caller has no next-fire time to record.
func (s *Store) RecordScheduleFire(ctx context.Context, id int64, lastJobID *int64, nextAt *time.Time) error {
	if lastJobID == nil && nextAt == nil {
		return nil
	}

	var (
		query string
		args  []any
	)
	switch {
	case lastJobID != nil && nextAt != nil:
		query = `UPDATE schedule SET last_job_id = ?, next_at = ? WHERE id = ?`
		args = []any{*lastJobID, nextAt.UTC().UnixMilli(), id}
	case lastJobID != nil:
		query = `UPDATE schedule SET last_job_id = ? WHERE id = ?`
		args = []any{*lastJobID, id}
	default:
		query = `UPDATE schedule SET next_at = ? WHERE id = ?`
		args = []any{nextAt.UTC().UnixMilli(), id}
	}

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return wrap("record schedule fire", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("record schedule fire", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: record schedule fire %d: %w", id, ErrNotFound)
	}
	return nil
}

func scanSchedule(row rowScanner) (Schedule, error) {
	var (
		sc        Schedule
		argsJSON  string
		enabled   int
		jitterS   int64
		lastJobID sql.NullInt64
		nextAt    sql.NullInt64
	)
	err := row.Scan(&sc.ID, &sc.Name, &sc.CronExpr, &sc.Kind, &argsJSON, &sc.ConcurrencyKey,
		&jitterS, &enabled, &lastJobID, &nextAt)
	if err != nil {
		return Schedule{}, err
	}

	sc.JitterS = int(jitterS)
	sc.Enabled = enabled != 0
	sc.LastJobID = int64Ptr(lastJobID)
	sc.NextAt = timePtr(nextAt)
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &sc.Args); err != nil {
			return Schedule{}, fmt.Errorf("decode args of schedule %d: %w", sc.ID, err)
		}
	}
	return sc, nil
}

func int64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
