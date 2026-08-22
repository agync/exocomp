package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func muteTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestCreateNotifyMuteRequiresFutureExpiry(t *testing.T) {
	st := muteTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := st.CreateNotifyMute(ctx, NotifyMute{ScopeKind: ScopeGlobal, CreatedAt: now}); err == nil {
		t.Fatal("expected an absent expiry to be refused")
	}
	if _, err := st.CreateNotifyMute(ctx, NotifyMute{
		ScopeKind: ScopeGlobal, CreatedAt: now, ExpiresAt: now.Add(-time.Minute),
	}); err == nil {
		t.Fatal("expected a past expiry to be refused")
	}
	if _, err := st.CreateNotifyMute(ctx, NotifyMute{
		ScopeKind: ScopeAlert, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err == nil {
		t.Fatal("expected an alert-scoped mute with no value to be refused")
	}
}

func TestNotifyMuteActiveOnlyAndDelete(t *testing.T) {
	st := muteTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	active, err := st.CreateNotifyMute(ctx, NotifyMute{
		ScopeKind: ScopeGlobal, Reason: "maintenance", CreatedVia: "cli",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create active mute: %v", err)
	}
	// An already-expired mute is stored by writing created/expires in the
	// past directly; CreateNotifyMute refuses a past expiry, so seed via the
	// active row then confirm activeOnly filtering with a short window.
	if _, err := st.CreateNotifyMute(ctx, NotifyMute{
		ScopeKind: ScopeHost, ScopeValue: "aurora",
		CreatedAt: now, ExpiresAt: now.Add(time.Millisecond),
	}); err != nil {
		t.Fatalf("create short mute: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	activeList, err := st.ListNotifyMutes(ctx, true)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(activeList) != 1 || activeList[0].ID != active.ID {
		t.Fatalf("active list = %+v, want only mute %d", activeList, active.ID)
	}

	all, err := st.ListNotifyMutes(ctx, false)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all list has %d, want 2", len(all))
	}

	if err := st.DeleteNotifyMute(ctx, active.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteNotifyMute(ctx, active.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}

func TestNotifyMuteMatches(t *testing.T) {
	global := NotifyMute{ScopeKind: ScopeGlobal}
	if !global.Matches("anything", "anywhere") {
		t.Fatal("global mute should match everything")
	}
	if !global.Matches("", "") {
		t.Fatal("global mute should match an unattributed notification")
	}

	alert := NotifyMute{ScopeKind: ScopeAlert, ScopeValue: "disk-pressure"}
	if !alert.Matches("disk-pressure", "aurora") {
		t.Fatal("alert mute should match its alert")
	}
	if alert.Matches("cpu-steal", "aurora") {
		t.Fatal("alert mute should not match a different alert")
	}
	if alert.Matches("", "aurora") {
		t.Fatal("alert mute should not match an empty alert")
	}

	host := NotifyMute{ScopeKind: ScopeHost, ScopeValue: "aurora"}
	if !host.Matches("disk-pressure", "aurora") {
		t.Fatal("host mute should match its host")
	}
	if host.Matches("disk-pressure", "edge-01") {
		t.Fatal("host mute should not match a different host")
	}
}
