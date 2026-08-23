package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// ExtraEgressPorts are TCP ports the operator has deliberately granted this
	// workspace's sandboxed tofu runs on top of the built-in HTTPS-and-backend
	// egress. Empty is the default posture. Because Landlock filters egress by
	// port and not by destination, a granted port opens that port to any host,
	// not only the intended one (docs/operations/serve.md).
	ExtraEgressPorts []int
}

// ErrInvalidEgressPort reports a declared egress port outside 1..65535.
var ErrInvalidEgressPort = errors.New("store: egress port out of range 1..65535")

// normalizeEgressPorts validates, de-duplicates, and sorts a declared egress
// port set. It rejects any port outside 1..65535 rather than silently dropping
// it: a junk declaration is an operator mistake worth surfacing at write time,
// not a posture to quietly widen or narrow.
func normalizeEgressPorts(ports []int) ([]int, error) {
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("%w: %d", ErrInvalidEgressPort, p)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
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

const workspaceColumns = `id, name, tofu_dir, auto_apply, serial, extra_egress_ports`

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

// SetWorkspaceEgressPorts replaces a workspace's declared extra egress ports.
// The set is validated (1..65535), de-duplicated, and sorted before it is
// persisted, so a junk declaration is refused here rather than reaching the
// sandbox profile. Passing an empty set restores the default posture.
func (s *Store) SetWorkspaceEgressPorts(ctx context.Context, id int64, ports []int) (Workspace, error) {
	clean, err := normalizeEgressPorts(ports)
	if err != nil {
		return Workspace{}, err
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		return Workspace{}, wrap("set egress ports", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE workspace SET extra_egress_ports = ? WHERE id = ?`, string(encoded), id)
	if err != nil {
		return Workspace{}, wrap("set egress ports", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Workspace{}, ErrNotFound
	}
	return s.GetWorkspace(ctx, id)
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

// PruneTFStates deletes a workspace's old state versions under a keep policy:
// the newest keepSerials versions are always retained, and so is every version
// created at or after keepSince. A version is pruned only when it fails both —
// old and beyond the serial window (expert-reviews.md #7: state grows unbounded
// otherwise, and every backup copies all serials). The latest version is always
// within the newest keepSerials, so a workspace never loses its current state.
// keepSerials below 1 is treated as 1.
func (s *Store) PruneTFStates(ctx context.Context, workspaceID int64, keepSerials int, keepSince time.Time) (int, error) {
	if keepSerials < 1 {
		keepSerials = 1
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM tfstate
		WHERE workspace_id = ?1
		  AND created_at < ?2
		  AND id NOT IN (
		      SELECT id FROM tfstate WHERE workspace_id = ?1
		      ORDER BY serial DESC, id DESC LIMIT ?3
		  )`,
		workspaceID, keepSince.UTC().UnixMilli(), keepSerials)
	if err != nil {
		return 0, wrap("prune tfstates", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, wrap("prune tfstates", err)
	}
	return int(n), nil
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

// ListTFLocks returns every held workspace lock, oldest first.
func (s *Store) ListTFLocks(ctx context.Context) ([]TFLock, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+tflockColumns+` FROM tflock ORDER BY created_at, workspace_id`)
	if err != nil {
		return nil, wrap("list tflocks", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TFLock
	for rows.Next() {
		l, err := scanTFLock(rows)
		if err != nil {
			return nil, wrap("list tflocks", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list tflocks", err)
	}
	return out, nil
}

// StaleTFLocks returns the locks whose owning job can no longer release them:
// the job reached a terminal state, or the lock names no job at all. A killed
// apply leaves exactly this, and nothing else in the system moves it (ADR
// 0004: stale-lock recovery is an audited force-unlock, flagged on boot).
//
// It reads the job table as it stands, so callers run it after the boot sweep
// has retired the rows a dead process left: a lock whose job is still running
// under a live process is not stale.
func (s *Store) StaleTFLocks(ctx context.Context) ([]TFLock, error) {
	locks, err := s.ListTFLocks(ctx)
	if err != nil {
		return nil, err
	}

	var stale []TFLock
	for _, l := range locks {
		if l.JobID == nil {
			stale = append(stale, l)
			continue
		}
		job, err := s.GetJob(ctx, *l.JobID)
		if errors.Is(err, ErrNotFound) {
			stale = append(stale, l)
			continue
		}
		if err != nil {
			return nil, err
		}
		if job.State.Terminal() {
			stale = append(stale, l)
		}
	}
	return stale, nil
}

// ForceReleaseTFLock drops a workspace's lock whoever holds it, and returns
// the lock it removed. A free workspace is ErrNotFound: there is nothing to
// record having broken.
func (s *Store) ForceReleaseTFLock(ctx context.Context, workspaceID int64) (TFLock, error) {
	held, err := s.GetTFLock(ctx, workspaceID)
	if err != nil {
		return TFLock{}, err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM tflock WHERE workspace_id = ?`, workspaceID); err != nil {
		return TFLock{}, wrap("force release tflock", err)
	}
	return held, nil
}

func scanWorkspace(row rowScanner) (Workspace, error) {
	var (
		w Workspace
		// auto_apply is reserved for the M3 scheduled-drift work and is read
		// only to consume the column; nothing sets it.
		autoApply int64
		egress    string
	)
	if err := row.Scan(&w.ID, &w.Name, &w.TofuDir, &autoApply, &w.Serial, &egress); err != nil {
		return Workspace{}, err
	}
	if egress != "" {
		if err := json.Unmarshal([]byte(egress), &w.ExtraEgressPorts); err != nil {
			return Workspace{}, fmt.Errorf("store: workspace %d egress ports: %w", w.ID, err)
		}
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
