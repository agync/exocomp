package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PushEndpoint is one registered UnifiedPush distributor URL and its health. The
// URL is client-supplied, so this row is only ever written after the web layer
// has validated it against the SSRF deny-list; the store keeps the fact, not the
// judgement. TokenID/TokenName attribute it to the operator token that
// registered it, so a device manages only its own.
type PushEndpoint struct {
	ID            int64
	TokenID       int64
	TokenName     string
	URL           string
	Label         string
	CreatedAt     time.Time
	LastSuccessAt *time.Time
	// LastError is the last delivery failure, or empty when the last send
	// succeeded (or none has run yet). It is a health hint, never a secret.
	LastError string
}

const pushEndpointColumns = `id, token_id, token_name, url, label, created_at, last_success_at, last_error`

// CreatePushEndpoint inserts an endpoint. The URL and the owning token are
// mandatory; the caller (web) has already validated the URL is a public https
// target before reaching here.
func (s *Store) CreatePushEndpoint(ctx context.Context, e PushEndpoint) (PushEndpoint, error) {
	if e.URL == "" {
		return PushEndpoint{}, errors.New("store: create push endpoint: no url")
	}
	if e.TokenID == 0 {
		return PushEndpoint{}, errors.New("store: create push endpoint: no owning token")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO push_endpoint (token_id, token_name, url, label, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		e.TokenID, e.TokenName, e.URL, e.Label, e.CreatedAt.UTC().UnixMilli())
	if err != nil {
		return PushEndpoint{}, wrap("create push endpoint", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return PushEndpoint{}, wrap("create push endpoint", err)
	}
	e.ID = id
	return e, nil
}

// GetPushEndpoint returns one endpoint by id, or ErrNotFound. It is the lookup a
// scoped delete checks ownership against before removing the row.
func (s *Store) GetPushEndpoint(ctx context.Context, id int64) (PushEndpoint, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pushEndpointColumns+` FROM push_endpoint WHERE id = ?`, id)
	e, err := scanPushEndpoint(row)
	if err != nil {
		return PushEndpoint{}, wrap("get push endpoint", err)
	}
	return e, nil
}

// ListPushEndpoints returns every endpoint, newest first.
func (s *Store) ListPushEndpoints(ctx context.Context) ([]PushEndpoint, error) {
	return s.queryPushEndpoints(ctx,
		`SELECT `+pushEndpointColumns+` FROM push_endpoint ORDER BY created_at DESC, id DESC`)
}

// ListPushEndpointsByToken returns the endpoints a single token registered,
// newest first. It is what scopes a device's own listing.
func (s *Store) ListPushEndpointsByToken(ctx context.Context, tokenID int64) ([]PushEndpoint, error) {
	return s.queryPushEndpoints(ctx,
		`SELECT `+pushEndpointColumns+` FROM push_endpoint WHERE token_id = ? ORDER BY created_at DESC, id DESC`,
		tokenID)
}

// ActivePushEndpoints returns the endpoints the transport fans a notification
// out to. It is every registered row: an endpoint the distributor has dropped is
// self-pruned by the transport (a 404/410 deletes it), not filtered here.
func (s *Store) ActivePushEndpoints(ctx context.Context) ([]PushEndpoint, error) {
	return s.queryPushEndpoints(ctx,
		`SELECT `+pushEndpointColumns+` FROM push_endpoint ORDER BY id`)
}

// CountPushEndpointsForToken counts a token's endpoints, so the registration
// path can refuse one that would take the token past its cap.
func (s *Store) CountPushEndpointsForToken(ctx context.Context, tokenID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM push_endpoint WHERE token_id = ?`, tokenID).Scan(&n)
	if err != nil {
		return 0, wrap("count push endpoints", err)
	}
	return n, nil
}

// DeletePushEndpoint removes an endpoint by id, returning ErrNotFound when no
// row matched. The caller enforces ownership before calling.
func (s *Store) DeletePushEndpoint(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM push_endpoint WHERE id = ?`, id)
	if err != nil {
		return wrap("delete push endpoint", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete push endpoint", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete push endpoint %d: %w", id, ErrNotFound)
	}
	return nil
}

// MarkPushEndpointSuccess records a delivery that succeeded: it stamps
// last_success_at and clears any prior error.
func (s *Store) MarkPushEndpointSuccess(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE push_endpoint SET last_success_at = ?, last_error = NULL WHERE id = ?`,
		at.UTC().UnixMilli(), id)
	if err != nil {
		return wrap("mark push endpoint success", err)
	}
	return nil
}

// MarkPushEndpointError records why a delivery failed. An empty message clears
// to NULL, so a caller never has to special-case it.
func (s *Store) MarkPushEndpointError(ctx context.Context, id int64, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE push_endpoint SET last_error = ? WHERE id = ?`, nullStr(msg), id)
	if err != nil {
		return wrap("mark push endpoint error", err)
	}
	return nil
}

func (s *Store) queryPushEndpoints(ctx context.Context, query string, args ...any) ([]PushEndpoint, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list push endpoints", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PushEndpoint
	for rows.Next() {
		e, err := scanPushEndpoint(rows)
		if err != nil {
			return nil, wrap("list push endpoints", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list push endpoints", err)
	}
	return out, nil
}

func scanPushEndpoint(row rowScanner) (PushEndpoint, error) {
	var (
		e             PushEndpoint
		createdAt     int64
		lastSuccessAt sql.NullInt64
		lastError     sql.NullString
	)
	err := row.Scan(&e.ID, &e.TokenID, &e.TokenName, &e.URL, &e.Label,
		&createdAt, &lastSuccessAt, &lastError)
	if err != nil {
		return PushEndpoint{}, err
	}
	e.CreatedAt = time.UnixMilli(createdAt).UTC()
	e.LastSuccessAt = timePtr(lastSuccessAt)
	e.LastError = lastError.String
	return e, nil
}
