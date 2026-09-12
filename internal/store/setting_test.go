package store_test

import (
	"context"
	"errors"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestSettingRoundTrip pins the setting table's write/read/delete contract the
// dead-man lane relies on: PutSetting upserts, GetSetting returns the value
// (or ErrNotFound when unset), and DeleteSetting clears a row and is a no-op on
// an absent key.
func TestSettingRoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	if _, err := s.GetSetting(ctx, store.SettingDeadmanThreshold); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSetting on an absent key = %v, want ErrNotFound", err)
	}
	if err := s.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	got, err := s.GetSetting(ctx, store.SettingDeadmanThreshold)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if got != "48h" {
		t.Fatalf("GetSetting = %q, want 48h", got)
	}
	if err := s.PutSetting(ctx, store.SettingDeadmanThreshold, "72h"); err != nil {
		t.Fatalf("PutSetting upsert: %v", err)
	}
	if got, _ := s.GetSetting(ctx, store.SettingDeadmanThreshold); got != "72h" {
		t.Fatalf("GetSetting after upsert = %q, want 72h", got)
	}
	if err := s.DeleteSetting(ctx, store.SettingDeadmanThreshold); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	if _, err := s.GetSetting(ctx, store.SettingDeadmanThreshold); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSetting after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSetting(ctx, store.SettingDeadmanThreshold); err != nil {
		t.Fatalf("DeleteSetting on an absent key must be a no-op, got %v", err)
	}
}
