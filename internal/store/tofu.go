package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrLocked reports that a workspace's state lock is held by someone else, or
// that the caller presented a lock id the holder does not match.
var ErrLocked = errors.New("store: workspace state is locked")

// Workspace is one OpenTofu root module under this control plane's care.
type Workspace struct {
	ID      int64
	Name    string
	TofuDir string
	// Serial is the newest tfstate version stored for the workspace; 0 means
	// no state has ever been pushed.
	Serial int64
}

// TFState is one stored state version. Ciphertext is exactly the body tofu
// pushed: an envelope encrypted by tofu's own state encryption, opaque to
// every backup target and never decrypted here.
type TFState struct {
	ID           int64
	WorkspaceID  int64
	Serial       int64
	Lineage      string
	CreatedAt    time.Time
	CreatedByJob *int64
	Ciphertext   []byte
}

// NewTFState describes a state push. LockID is the lock the pusher claims to
// hold; SerialHint is the serial tofu wrote into the body, which the store
// keeps when it advances the workspace and replaces when it does not.
type NewTFState struct {
	WorkspaceID int64
	LockID      string
	SerialHint  int64
	Lineage     string
	JobID       *int64
	Ciphertext  []byte
}

// TFLock is the single row that locks a workspace.
type TFLock struct {
	WorkspaceID int64
	LockID      string
	Who         string
	Operation   string
	CreatedAt   time.Time
	JobID       *int64
}

const workspaceColumns = `id, name, tofu_dir, auto_apply, serial`

// CreateWorkspace inserts a workspace. A repeated name returns ErrDuplicate.
func (s *Store) CreateWorkspace(ctx context.Context, w Workspace) (Workspace, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace (name, tofu_dir, serial) VALUES (?, ?, 0)`,
		w.Name, w.TofuDir)
	if err != nil {
		return Workspace{}, wrap("create workspace", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Workspace{}, wrap("create workspace", err)
	}
	return s.GetWorkspace(ctx, id)
}

// GetWorkspace returns the workspace with the given id.
func (s *Store) GetWorkspace(ctx context.Context, id int64) (Workspace, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspace WHERE id = ?`, id)
	w, err := scanWorkspace(row)
	if err != nil {
		return Workspace{}, wrap("get workspace", err)
	}
	return w, nil
}

// WorkspaceByName returns the workspace with the given name, or ErrNotFound.
func (s *Store) WorkspaceByName(ctx context.Context, name string) (Workspace, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspace WHERE name = ?`, name)
	w, err := scanWorkspace(row)
	if err != nil {
		return Workspace{}, wrap("workspace by name", err)
	}
	return w, nil
}

// ListWorkspaces returns every workspace by name.
func (s *Store) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspace ORDER BY name`)
	if err != nil {
		return nil, wrap("list workspaces", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, wrap("list workspaces", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list workspaces", err)
	}
	return out, nil
}

// AppendTFState stores a new state version, verifying the lock the pusher
// claims. Versions are never overwritten: every serial is kept, so state
// history and rollback are ordinary rows.
//
// The serial stored is the higher of the workspace's next serial and the one
// tofu wrote into the body. Taking tofu's number keeps the row aligned with
// what a reader would find inside the ciphertext; taking ours keeps the column
// strictly increasing when tofu reuses a number, which the UNIQUE index would
// otherwise reject mid-apply.
func (s *Store) AppendTFState(ctx context.Context, in NewTFState) (TFState, error) {
	if len(in.Ciphertext) == 0 {
		return TFState{}, errors.New("store: append tfstate: empty body")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TFState{}, wrap("append tfstate", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current int64
	err = tx.QueryRowContext(ctx, `SELECT serial FROM workspace WHERE id = ?`,
		in.WorkspaceID).Scan(&current)
	if err != nil {
		return TFState{}, wrap("append tfstate", err)
	}

	if err := checkLockTx(ctx, tx, in.WorkspaceID, in.LockID); err != nil {
		return TFState{}, err
	}

	serial := current + 1
	if in.SerialHint > serial {
		serial = in.SerialHint
	}
	now := time.Now().UTC()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO tfstate (workspace_id, serial, lineage, created_at,
		                     created_by_job_id, ciphertext)
		VALUES (?, ?, ?, ?, ?, ?)`,
		in.WorkspaceID, serial, in.Lineage, now.UnixMilli(),
		nullInt64(in.JobID), in.Ciphertext)
	if err != nil {
		return TFState{}, wrap("append tfstate", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return TFState{}, wrap("append tfstate", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspace SET serial = ? WHERE id = ?`, serial, in.WorkspaceID); err != nil {
		return TFState{}, wrap("append tfstate", err)
	}
	if err := tx.Commit(); err != nil {
		return TFState{}, wrap("append tfstate", err)
	}

	return TFState{
		ID:           id,
		WorkspaceID:  in.WorkspaceID,
		Serial:       serial,
		Lineage:      in.Lineage,
		CreatedAt:    now,
		CreatedByJob: in.JobID,
		Ciphertext:   in.Ciphertext,
	}, nil
}

// checkLockTx enforces the lock contract for a state push: a caller naming a
// lock must be its holder, and a caller naming none must find the workspace
// unlocked.
func checkLockTx(ctx context.Context, tx *sql.Tx, workspaceID int64, lockID string) error {
	var held string
	err := tx.QueryRowContext(ctx,
		`SELECT lock_id FROM tflock WHERE workspace_id = ?`, workspaceID).Scan(&held)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if lockID == "" {
			return nil
		}
		return fmt.Errorf("store: append tfstate: workspace %d holds no lock %q: %w",
			workspaceID, lockID, ErrLocked)
	case err != nil:
		return wrap("append tfstate", err)
	case held == lockID:
		return nil
	default:
		return fmt.Errorf("store: append tfstate: workspace %d is locked by another holder: %w",
			workspaceID, ErrLocked)
	}
}

const tfstateColumns = `id, workspace_id, serial, lineage, created_at,
	created_by_job_id, ciphertext`

// LatestTFState returns the newest stored version, or ErrNotFound when the
// workspace has never been applied.
func (s *Store) LatestTFState(ctx context.Context, workspaceID int64) (TFState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+tfstateColumns+`
		FROM tfstate WHERE workspace_id = ? ORDER BY serial DESC, id DESC LIMIT 1`,
		workspaceID)
	st, err := scanTFState(row)
	if err != nil {
		return TFState{}, wrap("latest tfstate", err)
	}
	return st, nil
}

// ListTFStates returns a workspace's versions, newest first. A limit of zero
// returns all of them.
func (s *Store) ListTFStates(ctx context.Context, workspaceID int64, limit int) ([]TFState, error) {
	query := `SELECT ` + tfstateColumns + `
		FROM tfstate WHERE workspace_id = ? ORDER BY serial DESC, id DESC`
	args := []any{workspaceID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list tfstates", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TFState
	for rows.Next() {
		st, err := scanTFState(rows)
		if err != nil {
			return nil, wrap("list tfstates", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list tfstates", err)
	}
	return out, nil
}

const tflockColumns = `workspace_id, lock_id, who, operation, created_at, job_id`

// AcquireTFLock takes the workspace lock. Acquisition is the INSERT itself:
// the primary key admits one row, so SQLite picks the winner and every loser
// gets ErrLocked along with the holder it lost to.
func (s *Store) AcquireTFLock(ctx context.Context, l TFLock) (TFLock, error) {
	if l.LockID == "" {
		return TFLock{}, errors.New("store: acquire tflock: empty lock id")
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now().UTC()
	}

	// A holder that releases between the rejected INSERT and the lookup would
	// otherwise report a lock nobody holds; the free slot is worth one retry.
	for attempt := 0; attempt < 3; attempt++ {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO tflock (workspace_id, lock_id, who, operation, created_at, job_id)
			VALUES (?, ?, ?, ?, ?, ?)`,
			l.WorkspaceID, l.LockID, l.Who, l.Operation,
			l.CreatedAt.UTC().UnixMilli(), nullInt64(l.JobID))
		if err == nil {
			return l, nil
		}
		if wrapped := wrap("acquire tflock", err); !errors.Is(wrapped, ErrDuplicate) {
			return TFLock{}, wrapped
		}

		holder, err := s.GetTFLock(ctx, l.WorkspaceID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return TFLock{}, err
		}
		return holder, fmt.Errorf("store: acquire tflock: workspace %d held by %q: %w",
			l.WorkspaceID, holder.LockID, ErrLocked)
	}
	return TFLock{}, fmt.Errorf("store: acquire tflock: workspace %d contended: %w",
		l.WorkspaceID, ErrLocked)
}

// GetTFLock returns the lock on a workspace, or ErrNotFound when it is free.
func (s *Store) GetTFLock(ctx context.Context, workspaceID int64) (TFLock, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+tflockColumns+` FROM tflock WHERE workspace_id = ?`, workspaceID)
	l, err := scanTFLock(row)
	if err != nil {
		return TFLock{}, wrap("get tflock", err)
	}
	return l, nil
}

// ReleaseTFLock drops the lock, refusing when lockID is not the holder's.
// Releasing a free workspace is not an error: unlock is idempotent, and a
// retried unlock must not fail a job that already finished.
func (s *Store) ReleaseTFLock(ctx context.Context, workspaceID int64, lockID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tflock WHERE workspace_id = ? AND lock_id = ?`, workspaceID, lockID)
	if err != nil {
		return wrap("release tflock", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("release tflock", err)
	}
	if affected == 1 {
		return nil
	}

	holder, err := s.GetTFLock(ctx, workspaceID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("store: release tflock: workspace %d held by %q, not %q: %w",
		workspaceID, holder.LockID, lockID, ErrLocked)
}

func scanWorkspace(row rowScanner) (Workspace, error) {
	var (
		w Workspace
		// auto_apply is reserved for the M3 scheduled-drift work and is read
		// only to consume the column; nothing sets it.
		autoApply int64
	)
	if err := row.Scan(&w.ID, &w.Name, &w.TofuDir, &autoApply, &w.Serial); err != nil {
		return Workspace{}, err
	}
	return w, nil
}

func scanTFState(row rowScanner) (TFState, error) {
	var (
		st        TFState
		createdAt int64
		jobID     sql.NullInt64
	)
	err := row.Scan(&st.ID, &st.WorkspaceID, &st.Serial, &st.Lineage, &createdAt,
		&jobID, &st.Ciphertext)
	if err != nil {
		return TFState{}, err
	}
	st.CreatedAt = time.UnixMilli(createdAt).UTC()
	st.CreatedByJob = int64Ptr(jobID)
	return st, nil
}

func scanTFLock(row rowScanner) (TFLock, error) {
	var (
		l         TFLock
		createdAt int64
		jobID     sql.NullInt64
	)
	err := row.Scan(&l.WorkspaceID, &l.LockID, &l.Who, &l.Operation, &createdAt, &jobID)
	if err != nil {
		return TFLock{}, err
	}
	l.CreatedAt = time.UnixMilli(createdAt).UTC()
	l.JobID = int64Ptr(jobID)
	return l, nil
}

func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
