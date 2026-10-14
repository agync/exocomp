package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Token scopes. operator drives runs and approvals; approve can only decide
// pending approvals; readonly sees the surfaces and nothing that mutates.
const (
	ScopeOperator  = "operator"
	ScopeReadonly  = "readonly"
	ScopeApprove   = "approve"
	ScopeHostToken = "host"
)

// APIToken is one LAN access credential at rest. The secret itself is never a
// column: Hash is its argon2id PHC string, and Prefix is a non-secret handle
// shown in listings. A row is revoked by stamping RevokedAt, not by deleting,
// so its name stays reserved and sessions bound to it can be checked dead.
type APIToken struct {
	ID         int64
	Name       string
	Scope      string
	Host       string
	Prefix     string
	Hash       string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	RotatedAt  *time.Time
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

// CredentialEpoch is the millisecond instant the row's current secret was
// issued: the rotation time if the token has ever been rotated, else its
// creation time. A browser session is pinned to this value, so a rotate (which
// advances it) invalidates sessions minted against the superseded secret.
func (t APIToken) CredentialEpoch() int64 {
	if t.RotatedAt != nil {
		return t.RotatedAt.UnixMilli()
	}
	return t.CreatedAt.UnixMilli()
}

const apiTokenColumns = `id, name, scope, host, prefix, hash, created_at,
	expires_at, last_used_at, revoked_at, rotated_at`

// CreateToken inserts a token row. A duplicate name returns ErrDuplicate.
func (s *Store) CreateToken(ctx context.Context, t APIToken) (APIToken, error) {
	switch t.Scope {
	case ScopeOperator, ScopeReadonly, ScopeApprove, ScopeHostToken:
	default:
		return APIToken{}, fmt.Errorf("store: create token: scope %q is not %s, %s, %s, or %s",
			t.Scope, ScopeOperator, ScopeReadonly, ScopeApprove, ScopeHostToken)
	}
	if t.Scope == ScopeHostToken && strings.TrimSpace(t.Host) == "" {
		return APIToken{}, errors.New("store: create token: host scope requires a host binding")
	}
	if t.Name == "" {
		return APIToken{}, errors.New("store: create token: a name is mandatory")
	}
	if t.Hash == "" {
		return APIToken{}, errors.New("store: create token: no hash")
	}
	t.Host = strings.TrimSpace(t.Host)
	if t.Scope == ScopeHostToken {
		if !validHostBinding(t.Host) {
			return APIToken{}, errors.New("store: create token: host binding contains whitespace or separators")
		}
	} else {
		t.Host = ""
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO api_token (name, scope, host, prefix, hash, created_at, expires_at, last_used_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.Scope, nullHost(t.Host), t.Prefix, t.Hash, t.CreatedAt.UnixMilli(),
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
// (Decision 0009).
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

// RotateToken reseals a fresh secret onto the existing named row: it replaces the
// hash and prefix, advances rotated_at, clears any revocation, and resets the
// last-used stamp — keeping the id, name, and scope so attribution stays
// continuous. setExpiry replaces the expiry (nil clears it); when false the
// existing expiry is left untouched. A missing name returns ErrNotFound. The
// updated row is returned so the caller can format the new token string.
func (s *Store) RotateToken(ctx context.Context, name, hash, prefix string, expiresAt *time.Time, setExpiry bool, now time.Time) (APIToken, error) {
	if hash == "" {
		return APIToken{}, errors.New("store: rotate token: no hash")
	}
	query := `UPDATE api_token SET hash = ?, prefix = ?, rotated_at = ?, revoked_at = NULL, last_used_at = NULL`
	args := []any{hash, prefix, now.UTC().UnixMilli()}
	if setExpiry {
		query += `, expires_at = ?`
		args = append(args, msPtr(expiresAt))
	}
	query += ` WHERE name = ?`
	args = append(args, name)

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return APIToken{}, wrap("rotate token", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return APIToken{}, wrap("rotate token", err)
	}
	if affected == 0 {
		return APIToken{}, fmt.Errorf("store: rotate token %q: %w", name, ErrNotFound)
	}
	return s.GetTokenByName(ctx, name)
}

func scanToken(row rowScanner) (APIToken, error) {
	var (
		t                                           APIToken
		createdAt                                   int64
		expiresAt, lastUsedAt, revokedAt, rotatedAt sql.NullInt64
		host                                        sql.NullString
	)
	err := row.Scan(&t.ID, &t.Name, &t.Scope, &host, &t.Prefix, &t.Hash, &createdAt,
		&expiresAt, &lastUsedAt, &revokedAt, &rotatedAt)
	if err != nil {
		return APIToken{}, err
	}
	t.CreatedAt = time.UnixMilli(createdAt).UTC()
	t.ExpiresAt = timePtr(expiresAt)
	t.LastUsedAt = timePtr(lastUsedAt)
	t.RevokedAt = timePtr(revokedAt)
	t.RotatedAt = timePtr(rotatedAt)
	if host.Valid {
		t.Host = host.String
	}
	return t, nil
}

// nullHost turns an empty host binding into SQL NULL so non-host scopes store
// no per-machine identity.
func nullHost(h string) any {
	if strings.TrimSpace(h) == "" {
		return nil
	}
	return h
}

// validHostBinding rejects anything that could widen a host token's target:
// spaces/newlines and commas would let one token masquerade as a list of hosts.
func validHostBinding(h string) bool {
	if h == "" {
		return false
	}
	for _, r := range h {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ',' {
			return false
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
