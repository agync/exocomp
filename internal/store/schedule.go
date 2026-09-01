package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
	// LastRun is when the schedule last fired (nil = never). The scheduler
	// coalesces a catch-up fire off it after downtime.
	LastRun *time.Time
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
	jitter_s, enabled, last_job_id, next_at, last_run`

// CreateSchedule inserts an enabled schedule. Names are unique.
func (s *Store) CreateSchedule(ctx context.Context, n NewSchedule) (Schedule, error) {
	if !ValidName(n.Name) {
		return Schedule{}, fmt.Errorf("%w: store: schedule name %q must match [A-Za-z0-9_.-]+", ErrValidation, n.Name)
	}
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

// RecordScheduleFire stamps the outcome of one cron fire whose occurrence was
// safely enqueued (including a fire with no targets, which enqueues nothing but
// still happened): last_run advances to now so a reboot's catch-up sees the
// schedule as current. The scheduler withholds this call when an Enqueue failed,
// leaving last_run where it was so catch-up re-fires the dropped occurrence
// rather than skipping it. lastJobID is nil when the fire enqueued nothing (e.g.
// a backup schedule with no enabled targets); nextAt is nil when the caller has
// no next-fire time to record.
func (s *Store) RecordScheduleFire(ctx context.Context, id int64, lastJobID *int64, nextAt *time.Time) error {
	set := []string{"last_run = ?"}
	args := []any{time.Now().UTC().UnixMilli()}
	if lastJobID != nil {
		set = append(set, "last_job_id = ?")
		args = append(args, *lastJobID)
	}
	if nextAt != nil {
		set = append(set, "next_at = ?")
		args = append(args, nextAt.UTC().UnixMilli())
	}
	args = append(args, id)

	res, err := s.db.ExecContext(ctx,
		`UPDATE schedule SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...)
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

// SetScheduleLastRun overwrites a schedule's last_run. RecordScheduleFire
// covers the live path; this exists to seed last_run to a past instant so a
// test (or a migration/import) can model a daemon that was down across an
// occurrence.
func (s *Store) SetScheduleLastRun(ctx context.Context, id int64, t time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedule SET last_run = ? WHERE id = ?`, t.UTC().UnixMilli(), id)
	if err != nil {
		return wrap("set schedule last_run", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set schedule last_run", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set schedule last_run %d: %w", id, ErrNotFound)
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
		lastRun   sql.NullInt64
	)
	err := row.Scan(&sc.ID, &sc.Name, &sc.CronExpr, &sc.Kind, &argsJSON, &sc.ConcurrencyKey,
		&jitterS, &enabled, &lastJobID, &nextAt, &lastRun)
	if err != nil {
		return Schedule{}, err
	}

	sc.JitterS = int(jitterS)
	sc.Enabled = enabled != 0
	sc.LastJobID = int64Ptr(lastJobID)
	sc.NextAt = timePtr(nextAt)
	sc.LastRun = timePtr(lastRun)
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
