package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// KeyringEntry is one age recipient the data encryption key is wrapped for.
type KeyringEntry struct {
	ID         int64
	Kind       string // host|recovery|yubikey|extra
	Recipient  string // age1...
	WrappedDEK []byte
	Label      string
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

// Secret source kinds: how a sealed value was minted. A user secret was supplied
// by the operator on stdin; a generated secret was minted with crypto/rand and
// its previous value has no upstream to revoke.
const (
	SecretSourceUser      = "user"
	SecretSourceGenerated = "generated"
)

// SecretRow is a secret at rest: AES-256-GCM ciphertext under the DEK, with
// name|kind bound in as AAD. Plaintext never enters this package.
type SecretRow struct {
	ID         int64
	Name       string
	Kind       string
	Nonce      []byte
	Ciphertext []byte
	AAD        string
	// Source is how the value was minted (SecretSourceUser|SecretSourceGenerated).
	Source string
	// ExpiresAt is when the secret should be rotated or removed; nil means never.
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	RotatedAt  *time.Time
	LastUsedAt *time.Time
}

// CreateKeyringEntry adds a recipient to the keyring.
func (s *Store) CreateKeyringEntry(ctx context.Context, e KeyringEntry) (KeyringEntry, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO keyring (kind, recipient, wrapped_dek, label, created_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		e.Kind, e.Recipient, e.WrappedDEK, e.Label, e.CreatedAt.UnixMilli(), msPtr(e.RevokedAt))
	if err != nil {
		return KeyringEntry{}, wrap("create keyring entry", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return KeyringEntry{}, wrap("create keyring entry", err)
	}
	e.ID = id
	return e, nil
}

// GetKeyringEntry returns the keyring entry for a recipient.
func (s *Store) GetKeyringEntry(ctx context.Context, recipient string) (KeyringEntry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, kind, recipient, wrapped_dek, label, created_at, revoked_at
		FROM keyring WHERE recipient = ?`, recipient)
	e, err := scanKeyringEntry(row)
	if err != nil {
		return KeyringEntry{}, wrap("get keyring entry", err)
	}
	return e, nil
}

// ListKeyringEntries returns keyring entries; revoked ones only when
// includeRevoked is set.
func (s *Store) ListKeyringEntries(ctx context.Context, includeRevoked bool) ([]KeyringEntry, error) {
	query := `SELECT id, kind, recipient, wrapped_dek, label, created_at, revoked_at FROM keyring`
	if !includeRevoked {
		query += ` WHERE revoked_at IS NULL`
	}
	query += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, wrap("list keyring entries", err)
	}
	defer func() { _ = rows.Close() }()

	var out []KeyringEntry
	for rows.Next() {
		e, err := scanKeyringEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan keyring entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list keyring entries", err)
	}
	return out, nil
}

// RevokeKeyringEntry marks a recipient revoked; the row is kept as history.
func (s *Store) RevokeKeyringEntry(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE keyring SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return wrap("revoke keyring entry", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("revoke keyring entry", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: revoke keyring entry %d: %w", id, ErrNotFound)
	}
	return nil
}

func scanKeyringEntry(row rowScanner) (KeyringEntry, error) {
	var (
		e         KeyringEntry
		createdAt int64
		revokedAt sql.NullInt64
	)
	if err := row.Scan(&e.ID, &e.Kind, &e.Recipient, &e.WrappedDEK, &e.Label, &createdAt, &revokedAt); err != nil {
		return KeyringEntry{}, err
	}
	e.CreatedAt = time.UnixMilli(createdAt).UTC()
	e.RevokedAt = timePtr(revokedAt)
	return e, nil
}

// CreateSecret stores a sealed secret. Names are unique.
func (s *Store) CreateSecret(ctx context.Context, sec SecretRow) (SecretRow, error) {
	if sec.CreatedAt.IsZero() {
		sec.CreatedAt = time.Now().UTC()
	}
	if sec.Source == "" {
		sec.Source = SecretSourceUser
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO secret (name, kind, nonce, ciphertext, aad, source, expires_at, created_at, rotated_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sec.Name, sec.Kind, sec.Nonce, sec.Ciphertext, sec.AAD, sec.Source, msPtr(sec.ExpiresAt),
		sec.CreatedAt.UnixMilli(), msPtr(sec.RotatedAt), msPtr(sec.LastUsedAt))
	if err != nil {
		return SecretRow{}, wrap("create secret", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return SecretRow{}, wrap("create secret", err)
	}
	sec.ID = id
	return sec, nil
}

// GetSecret returns a secret by name.
func (s *Store) GetSecret(ctx context.Context, name string) (SecretRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, kind, nonce, ciphertext, aad, source, expires_at, created_at, rotated_at, last_used_at
		FROM secret WHERE name = ?`, name)
	sec, err := scanSecret(row)
	if err != nil {
		return SecretRow{}, wrap("get secret", err)
	}
	return sec, nil
}

// ListSecrets returns every secret row, ordered by name.
func (s *Store) ListSecrets(ctx context.Context) ([]SecretRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, kind, nonce, ciphertext, aad, source, expires_at, created_at, rotated_at, last_used_at
		FROM secret ORDER BY name`)
	if err != nil {
		return nil, wrap("list secrets", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SecretRow
	for rows.Next() {
		sec, err := scanSecret(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan secret: %w", err)
		}
		out = append(out, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list secrets", err)
	}
	return out, nil
}

// RotateSecret replaces the ciphertext of an existing secret and stamps
// rotated_at.
func (s *Store) RotateSecret(ctx context.Context, name string, nonce, ciphertext []byte) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE secret SET nonce = ?, ciphertext = ?, rotated_at = ? WHERE name = ?`,
		nonce, ciphertext, time.Now().UTC().UnixMilli(), name)
	if err != nil {
		return wrap("rotate secret", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("rotate secret", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: rotate secret %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetSecretExpiry sets (or with a nil at, clears) a secret's expiry. It is the
// lifecycle attribute the rotate path re-stamps; it never touches the sealed
// value.
func (s *Store) SetSecretExpiry(ctx context.Context, name string, at *time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE secret SET expires_at = ? WHERE name = ?`, msPtr(at), name)
	if err != nil {
		return wrap("set secret expiry", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set secret expiry", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set secret expiry %q: %w", name, ErrNotFound)
	}
	return nil
}

// TouchSecret records that a secret was used.
func (s *Store) TouchSecret(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE secret SET last_used_at = ? WHERE name = ?`,
		time.Now().UTC().UnixMilli(), name)
	if err != nil {
		return wrap("touch secret", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("touch secret", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: touch secret %q: %w", name, ErrNotFound)
	}
	return nil
}

// DeleteSecret removes a secret by name.
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secret WHERE name = ?`, name)
	if err != nil {
		return wrap("delete secret", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete secret", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete secret %q: %w", name, ErrNotFound)
	}
	return nil
}

func scanSecret(row rowScanner) (SecretRow, error) {
	var (
		sec                              SecretRow
		createdAt                        int64
		expiresAt, rotatedAt, lastUsedAt sql.NullInt64
	)
	err := row.Scan(&sec.ID, &sec.Name, &sec.Kind, &sec.Nonce, &sec.Ciphertext,
		&sec.AAD, &sec.Source, &expiresAt, &createdAt, &rotatedAt, &lastUsedAt)
	if err != nil {
		return SecretRow{}, err
	}
	sec.ExpiresAt = timePtr(expiresAt)
	sec.CreatedAt = time.UnixMilli(createdAt).UTC()
	sec.RotatedAt = timePtr(rotatedAt)
	sec.LastUsedAt = timePtr(lastUsedAt)
	return sec, nil
}
