// Package store owns the single SQLite file that holds every durable fact:
// jobs and their events, artifacts, the secrets keyring, and backup history.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// Errors returned by store operations. Callers match with errors.Is.
var (
	// ErrNotFound is returned when a lookup by id or unique key matches no row.
	ErrNotFound = errors.New("store: not found")
	// ErrIllegalTransition is returned when a job state move is not in the
	// state machine.
	ErrIllegalTransition = errors.New("store: illegal job state transition")
	// ErrStateConflict is returned when a job is not in the expected source
	// state, i.e. something else moved it first.
	ErrStateConflict = errors.New("store: job not in expected state")
	// ErrDuplicate is returned when a unique constraint rejects a write.
	ErrDuplicate = errors.New("store: duplicate key")
	// ErrConcurrencyHeld is returned when a claim is refused because a running
	// job already holds the row's concurrency key.
	ErrConcurrencyHeld = errors.New("store: concurrency key held")
	// ErrApprovalPending is returned when a claim is refused because the job
	// still carries an unsettled (pending) approval. It is the claim-path half
	// of the gate: a job queued while its approval is undecided must not run,
	// however it reached queued.
	ErrApprovalPending = errors.New("store: approval unsettled")
	// ErrValidation is returned when operator-input validation refuses a write
	// (a bad name, kind, or target shape) — a caller typo, not a store
	// failure, so the web surface flashes it instead of reporting a 500.
	ErrValidation = errors.New("store: validation")
)

// Store is the handle on the SQLite database.
type Store struct {
	db *sql.DB
}

// defaultBusyTimeout is how long a write waits for SQLite's lock before
// returning SQLITE_BUSY: ample for the two operator-scale processes that
// ever race a first open in practice (the CLI and the daemon), per
// docs/operations/serve.md.
const defaultBusyTimeout = 5 * time.Second

// Open opens (creating if needed) the SQLite database at path and migrates it
// to the current schema version.
func Open(path string) (*Store, error) {
	return openDSN(path, dsn(path))
}

// openDSN is Open with the connection string factored out, so a test can
// open the same file with a longer busy_timeout than the production default
// without duplicating the open sequence.
func openDSN(path, dsnStr string) (*Store, error) {
	db, err := sql.Open("sqlite", dsnStr)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// SQLite admits one writer at a time; a single pooled connection turns
	// contention into queueing instead of SQLITE_BUSY, and is ample for a
	// single-operator control plane.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: connect %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

func dsn(path string) string {
	return dsnWithBusyTimeout(path, defaultBusyTimeout)
}

func dsnWithBusyTimeout(path string, timeout time.Duration) string {
	pragmas := []string{
		fmt.Sprintf("_pragma=busy_timeout(%d)", timeout.Milliseconds()),
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=foreign_keys(ON)",
		// Every transaction starts as a writer, so a read-then-write
		// transaction cannot deadlock against another upgrading one.
		"_txlock=immediate",
	}
	return "file:" + url.PathEscape(path) + "?" + strings.Join(pragmas, "&")
}

// Pragma reads a connection pragma, e.g. "journal_mode".
func (s *Store) Pragma(ctx context.Context, name string) (string, error) {
	if !isBareIdent(name) {
		return "", fmt.Errorf("store: invalid pragma name %q", name)
	}
	var v string
	if err := s.db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil {
		return "", fmt.Errorf("store: pragma %s: %w", name, err)
	}
	return v, nil
}

func isBareIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '_' && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

func msPtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func intPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// wrap maps driver-level constraint failures onto the package errors.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("store: %s: %w: %v", op, ErrDuplicate, err)
	}
	return fmt.Errorf("store: %s: %w", op, err)
}
