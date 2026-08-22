package store

import (
	"context"
	"fmt"
	"time"
)

// Silence scopes, narrowest first. A global silence is the reactive kill
// switch: it swallows everything until it expires.
const (
	ScopeAlert  = "alert"
	ScopeHost   = "host"
	ScopeGlobal = "global"
)

// Silence swallows matching events until it expires. There is no
// never-expires form: a mute the operator forgets is how a homelab stops
// noticing it is broken, so ExpiresAt is mandatory in the schema and here.
type Silence struct {
	ID         int64
	ScopeKind  string
	ScopeValue string
	Reason     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	CreatedVia string
}

const silenceColumns = `id, scope_kind, scope_value, reason, created_at, expires_at, created_via`

// CreateSilence inserts a silence. An expiry that is absent or already past is
// refused rather than stored: a silence that swallows nothing is a lie on the
// banner.
func (s *Store) CreateSilence(ctx context.Context, sil Silence) (Silence, error) {
	switch sil.ScopeKind {
	case ScopeAlert, ScopeHost, ScopeGlobal:
	default:
		return Silence{}, fmt.Errorf("store: create silence: scope %q is not %s, %s, or %s",
			sil.ScopeKind, ScopeAlert, ScopeHost, ScopeGlobal)
	}
	if sil.ScopeKind != ScopeGlobal && sil.ScopeValue == "" {
		return Silence{}, fmt.Errorf("store: create silence: scope %s needs a value", sil.ScopeKind)
	}
	if sil.CreatedAt.IsZero() {
		sil.CreatedAt = time.Now().UTC()
	}
	if sil.ExpiresAt.IsZero() {
		return Silence{}, fmt.Errorf("store: create silence: an expiry is mandatory")
	}
	if !sil.ExpiresAt.After(sil.CreatedAt) {
		return Silence{}, fmt.Errorf("store: create silence: expiry %s is not after %s",
			sil.ExpiresAt.UTC().Format(time.RFC3339), sil.CreatedAt.UTC().Format(time.RFC3339))
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO silence (scope_kind, scope_value, reason, created_at, expires_at, created_via)
		VALUES (?, ?, ?, ?, ?, ?)`,
		sil.ScopeKind, sil.ScopeValue, sil.Reason,
		sil.CreatedAt.UTC().UnixMilli(), sil.ExpiresAt.UTC().UnixMilli(), sil.CreatedVia)
	if err != nil {
		return Silence{}, wrap("create silence", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Silence{}, wrap("create silence", err)
	}
	sil.ID = id
	return sil, nil
}

// ListSilences returns silences, newest first; activeOnly drops the expired
// ones. Expiry is decided by the database clock at read time, so nothing has
// to sweep for a silence to stop applying.
func (s *Store) ListSilences(ctx context.Context, activeOnly bool) ([]Silence, error) {
	query := `SELECT ` + silenceColumns + ` FROM silence`
	var args []any
	if activeOnly {
		query += ` WHERE expires_at > ?`
		args = append(args, time.Now().UTC().UnixMilli())
	}
	query += ` ORDER BY created_at DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list silences", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Silence
	for rows.Next() {
		sil, err := scanSilence(rows)
		if err != nil {
			return nil, wrap("list silences", err)
		}
		out = append(out, sil)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list silences", err)
	}
	return out, nil
}

// DeleteSilence lifts a silence by id.
func (s *Store) DeleteSilence(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM silence WHERE id = ?`, id)
	if err != nil {
		return wrap("delete silence", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete silence", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete silence %d: %w", id, ErrNotFound)
	}
	return nil
}

func scanSilence(row rowScanner) (Silence, error) {
	var (
		sil                  Silence
		createdAt, expiresAt int64
	)
	err := row.Scan(&sil.ID, &sil.ScopeKind, &sil.ScopeValue, &sil.Reason,
		&createdAt, &expiresAt, &sil.CreatedVia)
	if err != nil {
		return Silence{}, err
	}
	sil.CreatedAt = time.UnixMilli(createdAt).UTC()
	sil.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	return sil, nil
}
