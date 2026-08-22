package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestHoldStatusNamesPausedAndMuted proves doctor distinguishes the two holds:
// a global silence reads as PAUSED, an active notify mute as MUTED, and they
// are reported independently.
func TestHoldStatusNamesPausedAndMuted(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	dir := filepath.Join(root, "exocomp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()

	if got := holdStatus(ctx); got != "none active" {
		t.Fatalf("fresh store hold status = %q, want none active", got)
	}

	if _, err := st.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeGlobal, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create silence: %v", err)
	}
	if _, err := st.CreateNotifyMute(ctx, store.NotifyMute{
		ScopeKind: store.ScopeGlobal, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create mute: %v", err)
	}
	_ = st.Close()

	got := holdStatus(ctx)
	for _, want := range []string{"PAUSED (global silence)", "MUTED (1 active)"} {
		if !strings.Contains(got, want) {
			t.Errorf("hold status %q missing %q", got, want)
		}
	}
}

// TestProbeMetricsWithoutDaemon reports a not-running daemon as a state, not an
// error: no address file means nothing to scrape.
func TestProbeMetricsWithoutDaemon(t *testing.T) {
	if got := probeMetrics(context.Background(), t.TempDir()); got != "daemon not running (no address file)" {
		t.Fatalf("probeMetrics with no address file = %q", got)
	}
}
