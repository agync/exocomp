package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// JobState is a job's position in the execution state machine.
type JobState string

// The job states. awaiting_approval is reserved for M3 approvals; the queue
// never produces it in M1.
const (
	StateQueued           JobState = "queued"
	StateAwaitingApproval JobState = "awaiting_approval"
	StateRunning          JobState = "running"
	StateSucceeded        JobState = "succeeded"
	StateFailed           JobState = "failed"
	StateCancelled        JobState = "cancelled"
	StateTimedOut         JobState = "timed_out"
	StateInterrupted      JobState = "interrupted"
)

// jobTransitions is the whole state machine. States absent from the map are
// terminal.
var jobTransitions = map[JobState]map[JobState]bool{
	StateQueued: {
		StateRunning:          true,
		StateAwaitingApproval: true,
		StateCancelled:        true,
		StateInterrupted:      true,
	},
	StateAwaitingApproval: {
		StateQueued:      true,
		StateRunning:     true,
		StateCancelled:   true,
		StateInterrupted: true,
	},
	StateRunning: {
		StateSucceeded:   true,
		StateFailed:      true,
		StateCancelled:   true,
		StateTimedOut:    true,
		StateInterrupted: true,
	},
}

// Terminal reports whether the state admits no further transition.
func (s JobState) Terminal() bool {
	_, ok := jobTransitions[s]
	return !ok
}

// JobArgs is what a job needs to execute. It stands in for the M2 runbook
// table: in M1 a job carries its own entrypoint. Secret material never goes
// here — it is resolved at execution time and never written to the database.
type JobArgs struct {
	Argv    []string `json:"argv,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Timeout Duration `json:"timeout,omitempty"`
	// VaultProject names the envelope derivation an ansible job's vault
	// password comes from. The project name is public; the password it
	// derives is resolved when the job runs and never written here.
	VaultProject string `json:"vault_project,omitempty"`
}

// Duration is a time.Duration that marshals as a string ("30s") so job rows
// stay readable in plain SQLite.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Job is one row of the job table.
type Job struct {
	ID             int64
	Runbook        string
	Kind           string
	Trigger        string
	State          JobState
	Args           JobArgs
	QueuedAt       time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	ExitCode       *int
	ConcurrencyKey string
	DedupKey       string
	IdempotencyKey string
	OwnerPID       int
}

// NewJob describes a job to create. Every job starts queued.
type NewJob struct {
	Runbook        string
	Kind           string
	Trigger        string
	Args           JobArgs
	ConcurrencyKey string
	DedupKey       string
	IdempotencyKey string
}

const jobColumns = `id, runbook, kind, "trigger", state, args_json, queued_at,
	started_at, finished_at, exit_code, concurrency_key, dedup_key,
	idempotency_key, owner_pid`

// CreateJob inserts a queued job. A duplicate idempotency_key returns
// ErrDuplicate; use JobByIdempotencyKey to recover the original.
func (s *Store) CreateJob(ctx context.Context, n NewJob) (Job, error) {
	args, err := json.Marshal(n.Args)
	if err != nil {
		return Job{}, fmt.Errorf("store: create job: encode args: %w", err)
	}
	now := time.Now().UTC()

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO job (runbook, kind, "trigger", state, args_json, queued_at,
		                 concurrency_key, dedup_key, idempotency_key, owner_pid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.Runbook, n.Kind, n.Trigger, string(StateQueued), string(args),
		now.UnixMilli(), n.ConcurrencyKey, nullStr(n.DedupKey),
		nullStr(n.IdempotencyKey), os.Getpid())
	if err != nil {
		return Job{}, wrap("create job", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Job{}, wrap("create job", err)
	}
	return s.GetJob(ctx, id)
}

// GetJob returns the job with the given id.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM job WHERE id = ?`, id)
	job, err := scanJob(row)
	if err != nil {
		return Job{}, wrap("get job", err)
	}
	return job, nil
}

// JobByIdempotencyKey returns the job created under key, or ErrNotFound.
func (s *Store) JobByIdempotencyKey(ctx context.Context, key string) (Job, error) {
	if key == "" {
		return Job{}, fmt.Errorf("store: job by idempotency key: %w", ErrNotFound)
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM job WHERE idempotency_key = ?`, key)
	job, err := scanJob(row)
	if err != nil {
		return Job{}, wrap("job by idempotency key", err)
	}
	return job, nil
}

// JobFilter narrows ListJobs. A zero filter lists every job, newest first.
type JobFilter struct {
	States         []JobState
	ConcurrencyKey string
	Limit          int
}

// ListJobs returns jobs matching the filter, oldest queued first.
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	query := `SELECT ` + jobColumns + ` FROM job`
	var where []string
	var args []any

	if len(f.States) > 0 {
		placeholders := make([]string, len(f.States))
		for i, st := range f.States {
			placeholders[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	if f.ConcurrencyKey != "" {
		where = append(where, "concurrency_key = ?")
		args = append(args, f.ConcurrencyKey)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY queued_at, id"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list jobs", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, wrap("list jobs", err)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list jobs", err)
	}
	return out, nil
}

// TransitionJob is the only way a job changes state. It rejects moves the
// state machine does not allow (ErrIllegalTransition) and moves whose source
// state no longer matches (ErrStateConflict). Entering running stamps
// started_at and the owning pid; entering a terminal state stamps finished_at.
func (s *Store) TransitionJob(ctx context.Context, id int64, from, to JobState) error {
	if !jobTransitions[from][to] {
		return fmt.Errorf("store: %s -> %s: %w", from, to, ErrIllegalTransition)
	}

	now := time.Now().UTC().UnixMilli()
	var (
		query string
		args  []any
	)
	switch {
	case to == StateRunning:
		query = `UPDATE job SET state = ?, started_at = COALESCE(started_at, ?), owner_pid = ?
		         WHERE id = ? AND state = ?`
		args = []any{string(to), now, os.Getpid(), id, string(from)}
	case to.Terminal():
		query = `UPDATE job SET state = ?, finished_at = ?, owner_pid = 0
		         WHERE id = ? AND state = ?`
		args = []any{string(to), now, id, string(from)}
	default:
		query = `UPDATE job SET state = ? WHERE id = ? AND state = ?`
		args = []any{string(to), id, string(from)}
	}

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return wrap("transition job", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("transition job", err)
	}
	if affected == 1 {
		return nil
	}

	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	return fmt.Errorf("store: job %d is %s, not %s: %w", id, job.State, from, ErrStateConflict)
}

// ClaimJob moves a queued job to running, refusing when a running row already
// holds the same concurrency key. The key check and the transition are one
// statement, so the database — not a process's memory — decides who runs: a
// daemon and a CLI over the same file cannot both find the key free.
//
// A refusal that is not the job's state is ErrConcurrencyHeld, which a caller
// answers by trying another row rather than by waiting.
func (s *Store) ClaimJob(ctx context.Context, id int64) error {
	now := time.Now().UTC().UnixMilli()
	res, err := s.db.ExecContext(ctx, `
		UPDATE job SET state = ?, started_at = COALESCE(started_at, ?), owner_pid = ?
		WHERE id = ? AND state = ?
		  AND (concurrency_key = '' OR NOT EXISTS (
		        SELECT 1 FROM job AS held
		        WHERE held.concurrency_key = job.concurrency_key
		          AND held.state = ? AND held.id <> job.id))`,
		string(StateRunning), now, os.Getpid(), id, string(StateQueued), string(StateRunning))
	if err != nil {
		return wrap("claim job", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("claim job", err)
	}
	if affected == 1 {
		return nil
	}

	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if job.State != StateQueued {
		return fmt.Errorf("store: job %d is %s, not %s: %w",
			id, job.State, StateQueued, ErrStateConflict)
	}
	return fmt.Errorf("store: claim job %d: concurrency key %q is held: %w",
		id, job.ConcurrencyKey, ErrConcurrencyHeld)
}

// SetJobExitCode records the subprocess exit code. Correctness is decided by
// this number alone; the state transition that follows reflects it.
func (s *Store) SetJobExitCode(ctx context.Context, id int64, code int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE job SET exit_code = ? WHERE id = ?`, code, id)
	if err != nil {
		return wrap("set job exit code", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set job exit code", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set job exit code %d: %w", id, ErrNotFound)
	}
	return nil
}

// processStart is when this process loaded the package. A row owned by our own
// pid but stamped before this instant belongs to a previous process wearing
// the same number — the normal case in a container, where every restart is
// pid 1.
var processStart = time.Now().UTC()

// ownerAlive reports whether the process that owns a job row is still running.
// since is the row's own clock: started_at, or queued_at for a row that never
// started.
//
// Pid equality is not ownership. A container restart hands the new process the
// same pid 1 the dead one had, so a sweep keyed on "some other pid" leaves
// exactly the rows it exists to clean up.
func ownerAlive(pid int, since time.Time) bool {
	switch {
	case pid <= 0:
		// A terminal row, or one written before owner_pid meant anything.
		return false
	case pid == os.Getpid():
		return since.After(processStart)
	}
	// Signal 0 performs the permission and existence checks without sending
	// anything. EPERM means the process exists and is someone else's.
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// SweepInterrupted moves queued and running jobs whose owning process is gone
// to interrupted, a terminal state that is never auto-retried. One operator
// means one process runs jobs at a time (AGENTS.md invariant 1); this decides
// "gone" by asking the kernel, not by comparing pids.
func (s *Store) SweepInterrupted(ctx context.Context) (int, error) {
	rows, err := s.ListJobs(ctx, JobFilter{States: []JobState{StateQueued, StateRunning}})
	if err != nil {
		return 0, err
	}

	swept := 0
	for _, job := range rows {
		since := job.QueuedAt
		if job.StartedAt != nil {
			since = *job.StartedAt
		}
		if ownerAlive(job.OwnerPID, since) {
			continue
		}
		res, err := s.db.ExecContext(ctx, `
			UPDATE job SET state = ?, finished_at = ?, owner_pid = 0
			WHERE id = ? AND state = ?`,
			string(StateInterrupted), time.Now().UTC().UnixMilli(), job.ID, string(job.State))
		if err != nil {
			return swept, wrap("sweep interrupted", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return swept, wrap("sweep interrupted", err)
		}
		swept += int(affected)
	}
	return swept, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var (
		job                        Job
		state, argsJSON            string
		queuedAt                   int64
		startedAt, finishedAt      sql.NullInt64
		exitCode                   sql.NullInt64
		dedupKey, idempotencyKey   sql.NullString
		ownerPID                   int64
		runbook, kind, triggerName string
	)
	err := row.Scan(&job.ID, &runbook, &kind, &triggerName, &state, &argsJSON,
		&queuedAt, &startedAt, &finishedAt, &exitCode, &job.ConcurrencyKey,
		&dedupKey, &idempotencyKey, &ownerPID)
	if err != nil {
		return Job{}, err
	}

	job.Runbook = runbook
	job.Kind = kind
	job.Trigger = triggerName
	job.State = JobState(state)
	job.QueuedAt = time.UnixMilli(queuedAt).UTC()
	job.StartedAt = timePtr(startedAt)
	job.FinishedAt = timePtr(finishedAt)
	job.ExitCode = intPtr(exitCode)
	job.OwnerPID = int(ownerPID)
	if dedupKey.Valid {
		job.DedupKey = dedupKey.String
	}
	if idempotencyKey.Valid {
		job.IdempotencyKey = idempotencyKey.String
	}
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &job.Args); err != nil {
			return Job{}, fmt.Errorf("decode args of job %d: %w", job.ID, err)
		}
	}
	return job, nil
}
