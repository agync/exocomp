package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Setting keys. Non-secret operator configuration only; anything sensitive
// belongs in the sealed secret table.
const (
	// SettingOperator is the operator's display name, shown on the surfaces and
	// recorded by the setup wizard.
	SettingOperator = "operator.display_name"
	// SettingNtfyTopic is the generated ntfy topic label, kept so the surfaces
	// and a re-run can show the same channel the wizard minted.
	SettingNtfyTopic = "notify.ntfy_topic"
)

// PutSetting upserts a non-secret configuration value.
func (s *Store) PutSetting(ctx context.Context, key, value string) error {
	if key == "" {
		return errors.New("store: put setting: empty key")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO setting (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().UnixMilli())
	if err != nil {
		return wrap("put setting", err)
	}
	return nil
}

// GetSetting returns a stored value, or ErrNotFound when the key is unset.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM setting WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: get setting %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return "", wrap("get setting", err)
	}
	return value, nil
}
