package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Token scopes. operator drives runs and approvals; approve can only decide
// pending approvals; readonly sees the surfaces and nothing that mutates.
const (
	ScopeOperator = "operator"
	ScopeReadonly = "readonly"
	ScopeApprove  = "approve"
)

// APIToken is one LAN access credential at rest. The secret itself is never a
// column: Hash is its argon2id PHC string, and Prefix is a non-secret handle
// shown in listings. A row is revoked by stamping RevokedAt, not by deleting,
// so its name stays reserved and sessions bound to it can be checked dead.
type APIToken struct {
	ID         int64
	Name       string
	Scope      string
	Prefix     string
	Hash       string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Active reports whether the token authenticates at now: not revoked and not
// past its expiry.
func (t APIToken) Active(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return false
	}
	return true
}

const apiTokenColumns = `id, name, scope, prefix, hash, created_at,
	expires_at, last_used_at, revoked_at`

// CreateToken inserts a token row. A duplicate name returns ErrDuplicate.
func (s *Store) CreateToken(ctx context.Context, t APIToken) (APIToken, error) {
	switch t.Scope {
	case ScopeOperator, ScopeReadonly, ScopeApprove:
	default:
		return APIToken{}, fmt.Errorf("store: create token: scope %q is not %s, %s, or %s",
			t.Scope, ScopeOperator, ScopeReadonly, ScopeApprove)
	}
	if t.Name == "" {
		return APIToken{}, errors.New("store: create token: a name is mandatory")
	}
	if t.Hash == "" {
		return APIToken{}, errors.New("store: create token: no hash")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO api_token (name, scope, prefix, hash, created_at, expires_at, last_used_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.Scope, t.Prefix, t.Hash, t.CreatedAt.UnixMilli(),
		msPtr(t.ExpiresAt), msPtr(t.LastUsedAt), msPtr(t.RevokedAt))
	if err != nil {
		return APIToken{}, wrap("create token", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return APIToken{}, wrap("create token", err)
	}
	t.ID = id
	return t, nil
}

// GetToken returns the token with the given id, or ErrNotFound.
func (s *Store) GetToken(ctx context.Context, id int64) (APIToken, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+apiTokenColumns+` FROM api_token WHERE id = ?`, id)
	t, err := scanToken(row)
	if err != nil {
		return APIToken{}, wrap("get token", err)
	}
	return t, nil
}

// GetTokenByName returns the token with the given name, or ErrNotFound.
func (s *Store) GetTokenByName(ctx context.Context, name string) (APIToken, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+apiTokenColumns+` FROM api_token WHERE name = ?`, name)
	t, err := scanToken(row)
	if err != nil {
		return APIToken{}, wrap("get token by name", err)
	}
	return t, nil
}

// ListTokens returns tokens oldest first; revoked ones only when
// includeRevoked is set.
func (s *Store) ListTokens(ctx context.Context, includeRevoked bool) ([]APIToken, error) {
	query := `SELECT ` + apiTokenColumns + ` FROM api_token`
	if !includeRevoked {
		query += ` WHERE revoked_at IS NULL`
	}
	query += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, wrap("list tokens", err)
	}
	defer func() { _ = rows.Close() }()

	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan token: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list tokens", err)
	}
	return out, nil
}

// CountActiveTokens counts tokens that authenticate at now: not revoked and not
// past their expiry. It is what a non-loopback bind checks before it refuses
// (ADR 0009).
func (s *Store) CountActiveTokens(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM api_token
		WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
		now.UTC().UnixMilli()).Scan(&n)
	if err != nil {
		return 0, wrap("count active tokens", err)
	}
	return n, nil
}

// TouchToken records that a token authenticated a request.
func (s *Store) TouchToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_token SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return wrap("touch token", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("touch token", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: touch token %d: %w", id, ErrNotFound)
	}
	return nil
}

// RevokeToken stamps a token revoked. Revoking is immediate and idempotent: a
// second revoke of an already-revoked row reports ErrNotFound so the caller can
// tell it changed nothing.
func (s *Store) RevokeToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_token SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`,
		time.Now().UTC().UnixMilli(), name)
	if err != nil {
		return wrap("revoke token", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("revoke token", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: revoke token %q: %w", name, ErrNotFound)
	}
	return nil
}

func scanToken(row rowScanner) (APIToken, error) {
	var (
		t                                APIToken
		createdAt                        int64
		expiresAt, lastUsedAt, revokedAt sql.NullInt64
	)
	err := row.Scan(&t.ID, &t.Name, &t.Scope, &t.Prefix, &t.Hash, &createdAt,
		&expiresAt, &lastUsedAt, &revokedAt)
	if err != nil {
		return APIToken{}, err
	}
	t.CreatedAt = time.UnixMilli(createdAt).UTC()
	t.ExpiresAt = timePtr(expiresAt)
	t.LastUsedAt = timePtr(lastUsedAt)
	t.RevokedAt = timePtr(revokedAt)
	return t, nil
}
