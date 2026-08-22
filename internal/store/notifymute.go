package store

import (
	"context"
	"fmt"
	"time"
)

// NotifyMute holds notifications for a scope until it expires. It is not a
// Silence: a silence stops an event from ever becoming a job, a mute lets the
// job run and only holds the message that would have announced it. The two
// are separate rows so the banner can render PAUSED, MUTED, or both.
//
// There is no never-expires form, for the same reason silences have none:
// ExpiresAt is mandatory in the schema and refused here when absent or past.
type NotifyMute struct {
	ID         int64
	ScopeKind  string
	ScopeValue string
	Reason     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	CreatedVia string
}

const notifyMuteColumns = `id, scope_kind, scope_value, reason, created_at, expires_at, created_via`

// Matches reports whether this mute covers a notification from the given alert
// name and host. A global mute matches everything; an alert- or host-scoped
// mute matches only its value. An empty candidate value never matches a scoped
// mute, so an unattributed notification is held only by a global mute.
func (m NotifyMute) Matches(alert, host string) bool {
	switch m.ScopeKind {
	case ScopeGlobal:
		return true
	case ScopeAlert:
		return m.ScopeValue != "" && m.ScopeValue == alert
	case ScopeHost:
		return m.ScopeValue != "" && m.ScopeValue == host
	default:
		return false
	}
}

// CreateNotifyMute inserts a mute. The expiry rules mirror CreateSilence: an
// absent or already-past expiry is a mute that mutes nothing, which is a lie
// on the banner, so it is refused rather than stored.
func (s *Store) CreateNotifyMute(ctx context.Context, m NotifyMute) (NotifyMute, error) {
	switch m.ScopeKind {
	case ScopeAlert, ScopeHost, ScopeGlobal:
	default:
		return NotifyMute{}, fmt.Errorf("store: create notify mute: scope %q is not %s, %s, or %s",
			m.ScopeKind, ScopeAlert, ScopeHost, ScopeGlobal)
	}
	if m.ScopeKind != ScopeGlobal && m.ScopeValue == "" {
		return NotifyMute{}, fmt.Errorf("store: create notify mute: scope %s needs a value", m.ScopeKind)
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.ExpiresAt.IsZero() {
		return NotifyMute{}, fmt.Errorf("store: create notify mute: an expiry is mandatory")
	}
	if !m.ExpiresAt.After(m.CreatedAt) {
		return NotifyMute{}, fmt.Errorf("store: create notify mute: expiry %s is not after %s",
			m.ExpiresAt.UTC().Format(time.RFC3339), m.CreatedAt.UTC().Format(time.RFC3339))
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO notify_mute (scope_kind, scope_value, reason, created_at, expires_at, created_via)
		VALUES (?, ?, ?, ?, ?, ?)`,
		m.ScopeKind, m.ScopeValue, m.Reason,
		m.CreatedAt.UTC().UnixMilli(), m.ExpiresAt.UTC().UnixMilli(), m.CreatedVia)
	if err != nil {
		return NotifyMute{}, wrap("create notify mute", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return NotifyMute{}, wrap("create notify mute", err)
	}
	m.ID = id
	return m, nil
}

// ListNotifyMutes returns mutes, newest first; activeOnly drops the expired
// ones. As with silences, expiry is decided by the database clock at read
// time, so nothing has to sweep for a mute to lift.
func (s *Store) ListNotifyMutes(ctx context.Context, activeOnly bool) ([]NotifyMute, error) {
	query := `SELECT ` + notifyMuteColumns + ` FROM notify_mute`
	var args []any
	if activeOnly {
		query += ` WHERE expires_at > ?`
		args = append(args, time.Now().UTC().UnixMilli())
	}
	query += ` ORDER BY created_at DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list notify mutes", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NotifyMute
	for rows.Next() {
		m, err := scanNotifyMute(rows)
		if err != nil {
			return nil, wrap("list notify mutes", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list notify mutes", err)
	}
	return out, nil
}

// DeleteNotifyMute lifts a mute by id before it expires.
func (s *Store) DeleteNotifyMute(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM notify_mute WHERE id = ?`, id)
	if err != nil {
		return wrap("delete notify mute", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete notify mute", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete notify mute %d: %w", id, ErrNotFound)
	}
	return nil
}

func scanNotifyMute(row rowScanner) (NotifyMute, error) {
	var (
		m                    NotifyMute
		createdAt, expiresAt int64
	)
	err := row.Scan(&m.ID, &m.ScopeKind, &m.ScopeValue, &m.Reason,
		&createdAt, &expiresAt, &m.CreatedVia)
	if err != nil {
		return NotifyMute{}, err
	}
	m.CreatedAt = time.UnixMilli(createdAt).UTC()
	m.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	return m, nil
}
