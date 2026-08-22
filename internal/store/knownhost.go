package store

import (
	"context"
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

// RecordKnownHost stores or replaces a host's key of one type. A repeat of the
// same (host, key_type) replaces the value: an operator-confirmed rekey, not a
// second row.
func (s *Store) RecordKnownHost(ctx context.Context, kh KnownHost) error {
	if kh.Host == "" || kh.KeyType == "" || kh.Key == "" {
		return fmt.Errorf("store: record known host: host, key_type and key are all required")
	}
	if kh.CreatedAt.IsZero() {
		kh.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO known_host (host, key_type, key, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(host, key_type) DO UPDATE SET key = excluded.key, created_at = excluded.created_at`,
		kh.Host, kh.KeyType, kh.Key, kh.CreatedAt.UnixMilli())
	if err != nil {
		return wrap("record known host", err)
	}
	return nil
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
