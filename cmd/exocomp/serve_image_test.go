package main

import (
	"context"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestRecordImageRefPersists: a start with EXOCOMP_IMAGE records the ref for
// the recovery chain; a later start without one leaves it alone, so a host
// restarted minus the env does not erase what RECOVERY.md names; a changed
// image updates it.
func TestRecordImageRefPersists(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	if err := recordImageRef(ctx, st, "ghcr.io/abn/exocomp:latest"); err != nil {
		t.Fatalf("record ref: %v", err)
	}
	if got, err := st.GetSetting(ctx, store.SettingImageRef); err != nil || got != "ghcr.io/abn/exocomp:latest" {
		t.Fatalf("stored ref = %q, %v", got, err)
	}

	if err := recordImageRef(ctx, st, ""); err != nil {
		t.Fatalf("record empty: %v", err)
	}
	if got, _ := st.GetSetting(ctx, store.SettingImageRef); got != "ghcr.io/abn/exocomp:latest" {
		t.Errorf("unset env overwrote the stored ref: %q", got)
	}

	if err := recordImageRef(ctx, st, "localhost/exocomp:dev"); err != nil {
		t.Fatalf("update ref: %v", err)
	}
	if got, _ := st.GetSetting(ctx, store.SettingImageRef); got != "localhost/exocomp:dev" {
		t.Errorf("a changed image did not update the ref: %q", got)
	}
}
