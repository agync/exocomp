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

	if err := recordImageRef(ctx, st, "ghcr.io/agync/exocomp:latest"); err != nil {
		t.Fatalf("record ref: %v", err)
	}
	if got, err := st.GetSetting(ctx, store.SettingImageRef); err != nil || got != "ghcr.io/agync/exocomp:latest" {
		t.Fatalf("stored ref = %q, %v", got, err)
	}

	if err := recordImageRef(ctx, st, ""); err != nil {
		t.Fatalf("record empty: %v", err)
	}
	if got, _ := st.GetSetting(ctx, store.SettingImageRef); got != "ghcr.io/agync/exocomp:latest" {
		t.Errorf("unset env overwrote the stored ref: %q", got)
	}

	if err := recordImageRef(ctx, st, "localhost/exocomp:dev"); err != nil {
		t.Fatalf("update ref: %v", err)
	}
	if got, _ := st.GetSetting(ctx, store.SettingImageRef); got != "localhost/exocomp:dev" {
		t.Errorf("a changed image did not update the ref: %q", got)
	}
}

// TestValidateCheckCanary: a malformed canary is refused at startup with a
// message naming it, so a typo cannot silently degrade every sweep to a missed
// observation; empty (disabled) and a real ip:port pass.
//
// Mutation check: drop the netip.ParseAddrPort refusal and "nope"/"192.168.1.1"
// are accepted, failing the bad-value loop below.
func TestValidateCheckCanary(t *testing.T) {
	for _, ok := range []string{"", "192.168.1.1:443", "[2001:db8::1]:53"} {
		if err := validateCheckCanary(ok); err != nil {
			t.Errorf("validateCheckCanary(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"nope", "192.168.1.1", "192.168.1.1:0", "192.168.1.1:70000", ":443"} {
		if err := validateCheckCanary(bad); err == nil {
			t.Errorf("validateCheckCanary(%q) = nil, want a refusal", bad)
		}
	}
}
