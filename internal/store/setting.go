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
	// SettingNtfyURL is the server URL for ntfy (e.g. https://ntfy.sh or self-hosted).
	SettingNtfyURL = "notify.ntfy_url"
	// SettingNtfyTopic is the generated ntfy topic label, kept so the surfaces
	// and a re-run can show the same channel the wizard minted.
	SettingNtfyTopic = "notify.ntfy_topic"
	// SettingNtfyDetail is how much a push names and whether it carries the
	// Approve/Reject buttons, for an operator who does not trust their
	// configured ntfy relay — or a lock screen — with the detail. See
	// notify.DetailFull, notify.DetailSummary and notify.DetailTerse.
	SettingNtfyDetail = "notify.ntfy_detail"
	// SettingPublicURL is the base URL `exocomp serve --public-url` was
	// started with, persisted so a separate `approval link` invocation can
	// read it back — the running daemon's in-memory copy (reactive.Engine's
	// baseURL) does not survive past that process.
	SettingPublicURL = "daemon.public_url"
	// SettingSecretStaging is the run-secret file staging backend
	// `exocomp serve --secret-staging` was started with (auto, tmpfs, or
	// disk), persisted so `exocomp doctor` reports the operator's configured
	// choice even when run against a stopped daemon's data dir, not only the
	// resolved posture a live process can probe.
	SettingSecretStaging = "secret.staging_backend"
	// SettingDeadmanThreshold is the dead-man trip threshold: how long without
	// a SUCCEEDED backup before the dead-man trips. A Go duration ("48h"); the
	// web Arm dialog writes one of the segmented values (24h/48h/72h/7d). A
	// duration that does not parse, or an empty value, means the dead-man is
	// NONE and nothing trips.
	SettingDeadmanThreshold = "deadman.threshold"
	// SettingDeadmanPagedAt is the "paged ntfy · once" marker: the unix-ms
	// timestamp of the last dead-man-tripped page, so the trip monitor pages
	// once per trip and every surface reads "paged ntfy · once · {age} ago".
	SettingDeadmanPagedAt = "deadman.paged_at"
	// SettingDeadmanPingedAt is the unix-ms timestamp of the last heartbeat
	// ping: every succeeded backup pings the sealed deadman/heartbeat_url, so
	// the card reads "pinged {age} ago" from this one value.
	SettingDeadmanPingedAt = "deadman.pinged_at"
	// SettingKitPrintedAt is the date the recovery kit was last printed
	// ("2026-08-14"), written by the /system reprint verb. Empty means the kit
	// has never been printed — the identity strip shows no kit meta at all.
	SettingKitPrintedAt = "kit.printed_at"
	// SettingKitChanges counts the invalidating changes since the last print —
	// a dead-man arm, a backup add-target, or a secret rotation (SPEC §7 A3).
	// The /system reprint verb resets it to zero.
	SettingKitChanges = "kit.changes"
	// SettingImageRef is the image this deployment runs, read from the unit's
	// EXOCOMP_IMAGE at startup. It is recorded so RECOVERY.md and `exocomp
	// doctor` name what a recovered host must load: a local image tag lives in
	// the store of the host that can fail (ADR 0016).
	SettingImageRef = "deploy.image_ref"
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

// DeleteSetting removes a stored value. Deleting an absent key is a no-op, so
// a disarm or a re-arm can clear a marker unconditionally.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("store: delete setting: empty key")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM setting WHERE key = ?`, key); err != nil {
		return wrap("delete setting", err)
	}
	return nil
}
