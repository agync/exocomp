package notify

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// captureTransport records every notification it is handed.
type captureTransport struct{ got []Notification }

func (c *captureTransport) Name() string { return "capture" }
func (c *captureTransport) Publish(_ context.Context, n Notification) error {
	c.got = append(c.got, n)
	return nil
}

func muteStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// A global mute holds the notification while leaving the Notify call reporting
// success: the mute is a deliberate quiet, not a delivery failure. This is the
// MUTED half of the banner — the announced job still ran.
func TestNotifyHeldByGlobalMute(t *testing.T) {
	st := muteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := st.CreateNotifyMute(ctx, store.NotifyMute{
		ScopeKind: store.ScopeGlobal, Reason: "maintenance",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create mute: %v", err)
	}

	sink := &captureTransport{}
	n := New(nil, sink).WithMutes(NewStoreMutes(st))
	if err := n.Notify(ctx, Notification{Title: "approval waiting", Source: "core-infra"}); err != nil {
		t.Fatalf("notify returned error: %v", err)
	}
	if len(sink.got) != 0 {
		t.Fatalf("transport received %d notifications under a global mute, want 0", len(sink.got))
	}
}

// Without a mute the same notification is delivered: the mute check is the only
// thing holding it, so lifting the mute must let it through unchanged.
func TestNotifyDeliversWhenUnmuted(t *testing.T) {
	st := muteStore(t)
	sink := &captureTransport{}
	n := New(nil, sink).WithMutes(NewStoreMutes(st))
	if err := n.Notify(context.Background(), Notification{Title: "approval waiting"}); err != nil {
		t.Fatalf("notify returned error: %v", err)
	}
	if len(sink.got) != 1 {
		t.Fatalf("transport received %d notifications with no mute, want 1", len(sink.got))
	}
}

// A scoped mute holds only what it covers: a host mute on aurora must not hold
// a notification from edge-01.
func TestNotifyScopedMuteMatchesOnlyItsScope(t *testing.T) {
	st := muteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := st.CreateNotifyMute(ctx, store.NotifyMute{
		ScopeKind: store.ScopeHost, ScopeValue: "aurora",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create mute: %v", err)
	}

	sink := &captureTransport{}
	n := New(nil, sink).WithMutes(NewStoreMutes(st))
	if err := n.Notify(ctx, Notification{Title: "disk", Host: "aurora"}); err != nil {
		t.Fatalf("notify aurora: %v", err)
	}
	if err := n.Notify(ctx, Notification{Title: "disk", Host: "edge-01"}); err != nil {
		t.Fatalf("notify edge-01: %v", err)
	}
	if len(sink.got) != 1 || sink.got[0].Host != "edge-01" {
		t.Fatalf("delivered = %+v, want only the edge-01 notification", sink.got)
	}
}
