package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// KnownHost is one recorded SSH host key for a managed host. It carries no
// secret: a host public key is public by construction, but recording it is what
// lets a fleet run verify the host instead of trusting it on first contact.
type KnownHost struct {
	Host      string
	KeyType   string
	Key       string
	CreatedAt time.Time
}

// ErrKnownHostChanged reports that a host presented a key different from the one
// already recorded for that key type. The recorded key is not overwritten: a
// changed key is a rekey the operator must confirm, or an attacker at scan time.
var ErrKnownHostChanged = errors.New("store: known host key changed")

// RecordKnownHost stores a host's key of one type on first sight (trust on first
// use). Re-recording the SAME key is idempotent. A DIFFERENT key for an existing
// (host, key_type) is refused with ErrKnownHostChanged — never silently
// overwritten; the caller confirms and calls RotateKnownHost to replace it.
func (s *Store) RecordKnownHost(ctx context.Context, kh KnownHost) error {
	if kh.Host == "" || kh.KeyType == "" || kh.Key == "" {
		return fmt.Errorf("store: record known host: host, key_type and key are all required")
	}
	if kh.CreatedAt.IsZero() {
		kh.CreatedAt = time.Now().UTC()
	}
	var existing string
	err := s.db.QueryRowContext(ctx,
		`SELECT key FROM known_host WHERE host = ? AND key_type = ?`, kh.Host, kh.KeyType).Scan(&existing)
	switch {
	case err == nil && existing == kh.Key:
		return nil
	case err == nil:
		return fmt.Errorf("%w: %s (%s)", ErrKnownHostChanged, kh.Host, kh.KeyType)
	case !errors.Is(err, sql.ErrNoRows):
		return wrap("record known host", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO known_host (host, key_type, key, created_at) VALUES (?, ?, ?, ?)`,
		kh.Host, kh.KeyType, kh.Key, kh.CreatedAt.UnixMilli()); err != nil {
		return wrap("record known host", err)
	}
	return nil
}

// RotateKnownHost replaces a recorded key after the operator has confirmed the
// change out of band (the --rotate path). It is also the approved re-pin that
// lifts a quarantine (fold 8d): the new key and the quarantine clear settle in
// one transaction, so a host is never left recording the new key while still
// blocked, nor cleared while still recording the old.
func (s *Store) RotateKnownHost(ctx context.Context, kh KnownHost) error {
	if kh.Host == "" || kh.KeyType == "" || kh.Key == "" {
		return fmt.Errorf("store: rotate known host: host, key_type and key are all required")
	}
	if kh.CreatedAt.IsZero() {
		kh.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrap("rotate known host", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO known_host (host, key_type, key, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(host, key_type) DO UPDATE SET key = excluded.key, created_at = excluded.created_at`,
		kh.Host, kh.KeyType, kh.Key, kh.CreatedAt.UnixMilli()); err != nil {
		return wrap("rotate known host", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM host_quarantine WHERE host = ?`, kh.Host); err != nil {
		return wrap("rotate known host", err)
	}
	if err := tx.Commit(); err != nil {
		return wrap("rotate known host", err)
	}
	return nil
}

// Fingerprint is the OpenSSH SHA256 fingerprint of a recorded key, so a
// quarantine record can carry the pinned key as the same string ssh prints for
// the presented one — old vs new, comparable at a glance. An unparseable key
// yields an empty string rather than an error: a fingerprint is a display aid,
// not a decision input.
func (kh KnownHost) Fingerprint() string {
	raw, err := base64.StdEncoding.DecodeString(kh.Key)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// HostQuarantine is a host blocked after a runtime host-key mismatch: the
// recorded fingerprint, the one the host presented, and why. It clears only
// through RotateKnownHost (the approved re-pin).
type HostQuarantine struct {
	Host           string
	Reason         string
	OldFingerprint string
	NewFingerprint string
	QuarantinedAt  time.Time
}

// QuarantineHost records (or refreshes) a host quarantine. It is idempotent by
// host: a repeated mismatch updates the record rather than failing, so a
// re-detected change does not error the run.
func (s *Store) QuarantineHost(ctx context.Context, q HostQuarantine) error {
	if q.Host == "" {
		return fmt.Errorf("store: quarantine host: host is required")
	}
	if q.QuarantinedAt.IsZero() {
		q.QuarantinedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_quarantine (host, reason, old_fingerprint, new_fingerprint, quarantined_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(host) DO UPDATE SET
			reason = excluded.reason,
			old_fingerprint = excluded.old_fingerprint,
			new_fingerprint = excluded.new_fingerprint,
			quarantined_at = excluded.quarantined_at`,
		q.Host, q.Reason, q.OldFingerprint, q.NewFingerprint, q.QuarantinedAt.UnixMilli())
	if err != nil {
		return wrap("quarantine host", err)
	}
	return nil
}

// GetHostQuarantine returns a host's quarantine, or ErrNotFound when the host is
// not quarantined.
func (s *Store) GetHostQuarantine(ctx context.Context, host string) (HostQuarantine, error) {
	var (
		q             HostQuarantine
		quarantinedAt int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT host, reason, old_fingerprint, new_fingerprint, quarantined_at
		FROM host_quarantine WHERE host = ?`, host).
		Scan(&q.Host, &q.Reason, &q.OldFingerprint, &q.NewFingerprint, &quarantinedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return HostQuarantine{}, fmt.Errorf("store: get host quarantine %q: %w", host, ErrNotFound)
	case err != nil:
		return HostQuarantine{}, wrap("get host quarantine", err)
	}
	q.QuarantinedAt = time.UnixMilli(quarantinedAt).UTC()
	return q, nil
}

// IsHostQuarantined reports whether a host is currently quarantined — the check
// the queue makes before admitting a run against a host's concurrency key.
func (s *Store) IsHostQuarantined(ctx context.Context, host string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM host_quarantine WHERE host = ?`, host).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, wrap("is host quarantined", err)
	}
	return true, nil
}

// ListQuarantinedHosts returns every quarantined host, newest first.
func (s *Store) ListQuarantinedHosts(ctx context.Context) ([]HostQuarantine, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT host, reason, old_fingerprint, new_fingerprint, quarantined_at
		FROM host_quarantine ORDER BY quarantined_at DESC, host`)
	if err != nil {
		return nil, wrap("list quarantined hosts", err)
	}
	defer func() { _ = rows.Close() }()

	var out []HostQuarantine
	for rows.Next() {
		var (
			q             HostQuarantine
			quarantinedAt int64
		)
		if err := rows.Scan(&q.Host, &q.Reason, &q.OldFingerprint, &q.NewFingerprint, &quarantinedAt); err != nil {
			return nil, wrap("list quarantined hosts", err)
		}
		q.QuarantinedAt = time.UnixMilli(quarantinedAt).UTC()
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list quarantined hosts", err)
	}
	return out, nil
}

// ListKnownHosts returns every recorded host key, ordered by host then type.
func (s *Store) ListKnownHosts(ctx context.Context) ([]KnownHost, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host, key_type, key, created_at FROM known_host ORDER BY host, key_type`)
	if err != nil {
		return nil, wrap("list known hosts", err)
	}
	defer func() { _ = rows.Close() }()

	var out []KnownHost
	for rows.Next() {
		var (
			kh        KnownHost
			createdAt int64
		)
		if err := rows.Scan(&kh.Host, &kh.KeyType, &kh.Key, &createdAt); err != nil {
			return nil, wrap("list known hosts", err)
		}
		kh.CreatedAt = time.UnixMilli(createdAt).UTC()
		out = append(out, kh)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list known hosts", err)
	}
	return out, nil
}

// DeleteKnownHost removes every recorded key for a host.
func (s *Store) DeleteKnownHost(ctx context.Context, host string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM known_host WHERE host = ?`, host)
	if err != nil {
		return wrap("delete known host", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete known host", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete known host %q: %w", host, ErrNotFound)
	}
	return nil
}

// RenderKnownHosts writes the rows as an OpenSSH known_hosts file body: one
// "host keytype key" line per entry, sorted, so ssh -o UserKnownHostsFile can
// verify a fleet run against it. The output is deterministic, which is what
// lets a run and a test agree on it.
func RenderKnownHosts(hosts []KnownHost) string {
	lines := make([]string, 0, len(hosts))
	for _, kh := range hosts {
		lines = append(lines, kh.Host+" "+kh.KeyType+" "+kh.Key)
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
