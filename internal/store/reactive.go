package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// EventOutcome is what the reactive pipeline decided about a received event.
// Every event carries one, including the ones nothing matched: "why didn't my
// rule fire?" is a query over this column, not an archaeology exercise.
type EventOutcome string

// The outcomes, in pipeline order.
const (
	// OutcomeIgnored: no enabled rule matched the event.
	OutcomeIgnored EventOutcome = "ignored"
	// OutcomeMatched: a rule matched and its action produces no job.
	OutcomeMatched EventOutcome = "matched"
	// OutcomeSuppressed: a guardrail swallowed the match.
	OutcomeSuppressed EventOutcome = "suppressed"
	// OutcomeGated: the action became an approval instead of a run.
	OutcomeGated EventOutcome = "gated"
	// OutcomeFired: the action reached the queue as a claimable job.
	OutcomeFired EventOutcome = "fired"
	// OutcomeError: the event could not be decided at all.
	OutcomeError EventOutcome = "error"
)

// Event is one received webhook, normalized, with what happened to it.
type Event struct {
	ID          int64
	Source      string
	ReceivedAt  time.Time
	DedupKey    string
	Normalized  string
	Raw         string
	MatchedRule string
	Outcome     EventOutcome
	Reason      string
	JobID       *int64
	// Trace is the per-condition evaluation trace, as JSON, exactly as it was
	// decided. It is stored rather than recomputed because the ledgers it read
	// move on afterwards.
	Trace string
}

const eventColumns = `id, source, received_at, dedup_key, normalized_json, raw_json,
	matched_rule, outcome, reason, job_id, trace_json`

// CreateEvent records an event and its outcome. Callers write the row once the
// pipeline has decided, so no event exists without a reason.
func (s *Store) CreateEvent(ctx context.Context, e Event) (Event, error) {
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now().UTC()
	}
	if e.Normalized == "" {
		e.Normalized = "{}"
	}
	if e.Raw == "" {
		e.Raw = "{}"
	}
	if e.Trace == "" {
		e.Trace = "[]"
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO event (source, received_at, dedup_key, normalized_json, raw_json,
		                   matched_rule, outcome, reason, job_id, trace_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Source, e.ReceivedAt.UTC().UnixMilli(), e.DedupKey, e.Normalized, e.Raw,
		e.MatchedRule, string(e.Outcome), e.Reason, nullInt64(e.JobID), e.Trace)
	if err != nil {
		return Event{}, wrap("create event", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, wrap("create event", err)
	}
	e.ID = id
	return e, nil
}

// GetEvent returns the event with the given id.
func (s *Store) GetEvent(ctx context.Context, id int64) (Event, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM event WHERE id = ?`, id)
	e, err := scanEvent(row)
	if err != nil {
		return Event{}, wrap("get event", err)
	}
	return e, nil
}

// EventFilter narrows ListEvents. A zero filter lists every event.
type EventFilter struct {
	Source   string
	Outcomes []EventOutcome
	Limit    int
}

// ListEvents returns events newest first.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	query := `SELECT ` + eventColumns + ` FROM event`
	var where []string
	var args []any

	if f.Source != "" {
		where = append(where, "source = ?")
		args = append(args, f.Source)
	}
	if len(f.Outcomes) > 0 {
		placeholders := make([]string, len(f.Outcomes))
		for i, o := range f.Outcomes {
			placeholders[i] = "?"
			args = append(args, string(o))
		}
		where = append(where, "outcome IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY received_at DESC, id DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list events", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, wrap("list events", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list events", err)
	}
	return out, nil
}

// actingOutcomes are the event outcomes that anchor the dedup window: an event
// that actually matched a rule and got as far as acting on it. An ignored event
// (no rule matched) or an errored one never fired anything, so it must not
// become the anchor that suppresses a later real alert sharing the key (finding
// R1: an inert plant would otherwise mute the alert). A suppressed duplicate is
// excluded too, so a source retrying forever cannot slide the window forward.
var actingOutcomes = []EventOutcome{OutcomeMatched, OutcomeGated, OutcomeFired}

// LastEventByDedupKey returns the newest event carrying key that actually
// matched a rule and acted on it, or ErrNotFound. It is the dedup ledger:
// anchoring on an event that never fired a rule would let an attacker plant an
// inert event to silence a real one, and anchoring on a suppressed duplicate
// would let a window slide forward forever on repeats.
func (s *Store) LastEventByDedupKey(ctx context.Context, key string) (Event, error) {
	if key == "" {
		return Event{}, fmt.Errorf("store: last event by dedup key: %w", ErrNotFound)
	}
	placeholders := make([]string, len(actingOutcomes))
	args := make([]any, 0, len(actingOutcomes)+1)
	args = append(args, key)
	for i, o := range actingOutcomes {
		placeholders[i] = "?"
		args = append(args, string(o))
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM event
		WHERE dedup_key = ? AND outcome IN (`+strings.Join(placeholders, ", ")+`)
		ORDER BY received_at DESC, id DESC LIMIT 1`, args...)
	e, err := scanEvent(row)
	if err != nil {
		return Event{}, wrap("last event by dedup key", err)
	}
	return e, nil
}

// EventByJobID returns the event that named a job, or ErrNotFound. A job is
// named by exactly one event — the one whose rule enqueued or gated it — so
// this recovers which rule and source raised a pending approval, which is how
// a resolve is scoped to the rule that owns the job it would cancel (R2).
func (s *Store) EventByJobID(ctx context.Context, jobID int64) (Event, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM event
		WHERE job_id = ? ORDER BY received_at DESC, id DESC LIMIT 1`, jobID)
	e, err := scanEvent(row)
	if err != nil {
		return Event{}, wrap("event by job id", err)
	}
	return e, nil
}

// CountEventsByDedupKey counts every event carrying key, whatever its outcome.
func (s *Store) CountEventsByDedupKey(ctx context.Context, key string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event WHERE dedup_key = ?`, key).Scan(&n)
	if err != nil {
		return 0, wrap("count events by dedup key", err)
	}
	return n, nil
}

// CountSuppressedSince counts the events swallowed at or after since, which is
// the number a silence banner carries.
func (s *Store) CountSuppressedSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event WHERE outcome = ? AND received_at >= ?`,
		string(OutcomeSuppressed), since.UTC().UnixMilli()).Scan(&n)
	if err != nil {
		return 0, wrap("count suppressed events", err)
	}
	return n, nil
}

// EventOutcomeCounts is every stored event, tallied by outcome. Total is the
// funnel's received count: every counted event has exactly one outcome, so it
// is the sum of the rest, never a separate query.
type EventOutcomeCounts struct {
	Ignored    int
	Matched    int
	Suppressed int
	Gated      int
	Fired      int
	Error      int
}

// Total reports every counted event, whatever its outcome.
func (c EventOutcomeCounts) Total() int {
	return c.Ignored + c.Matched + c.Suppressed + c.Gated + c.Fired + c.Error
}

// CountEventsByOutcome tallies every stored event by outcome in one grouped
// query over the same event table and outcome column ListEvents filters on,
// so a funnel summary built from it can never disagree with what ListEvents
// (and so `event list`/GET /events) show for the same rows.
func (s *Store) CountEventsByOutcome(ctx context.Context) (EventOutcomeCounts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT outcome, COUNT(*) FROM event GROUP BY outcome`)
	if err != nil {
		return EventOutcomeCounts{}, wrap("count events by outcome", err)
	}
	defer func() { _ = rows.Close() }()

	var c EventOutcomeCounts
	for rows.Next() {
		var outcome string
		var n int
		if err := rows.Scan(&outcome, &n); err != nil {
			return EventOutcomeCounts{}, wrap("count events by outcome", err)
		}
		switch EventOutcome(outcome) {
		case OutcomeIgnored:
			c.Ignored = n
		case OutcomeMatched:
			c.Matched = n
		case OutcomeSuppressed:
			c.Suppressed = n
		case OutcomeGated:
			c.Gated = n
		case OutcomeFired:
			c.Fired = n
		case OutcomeError:
			c.Error = n
		}
	}
	if err := rows.Err(); err != nil {
		return EventOutcomeCounts{}, wrap("count events by outcome", err)
	}
	return c, nil
}

func scanEvent(row rowScanner) (Event, error) {
	var (
		e          Event
		receivedAt int64
		outcome    string
		jobID      sql.NullInt64
	)
	err := row.Scan(&e.ID, &e.Source, &receivedAt, &e.DedupKey, &e.Normalized, &e.Raw,
		&e.MatchedRule, &outcome, &e.Reason, &jobID, &e.Trace)
	if err != nil {
		return Event{}, err
	}
	e.ReceivedAt = time.UnixMilli(receivedAt).UTC()
	e.Outcome = EventOutcome(outcome)
	e.JobID = int64Ptr(jobID)
	return e, nil
}
