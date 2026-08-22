package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestDashboardBannerMutedDistinctFromPaused proves the web banner aligns with
// the TUI model: a notify_mute renders MUTED (jobs still run) and a global
// silence renders PAUSED, as separate, correctly-labelled states.
func TestDashboardBannerMutedDistinctFromPaused(t *testing.T) {
	ctx := context.Background()

	t.Run("mute alone renders MUTED, not PAUSED", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.store.CreateNotifyMute(ctx, store.NotifyMute{
			ScopeKind: store.ScopeGlobal,
			Reason:    "maintenance window",
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create notify mute: %v", err)
		}
		page := getDashboardHTML(t, h)
		if !strings.Contains(page, "MUTED") {
			t.Errorf("dashboard missing MUTED banner:\n%s", bannerRegion(page))
		}
		if strings.Contains(page, "PAUSED") {
			t.Errorf("a mute must not read as PAUSED:\n%s", bannerRegion(page))
		}
		if !strings.Contains(page, "jobs still run") {
			t.Errorf("MUTED banner must state jobs still run:\n%s", bannerRegion(page))
		}
	})

	t.Run("global silence renders PAUSED", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.store.CreateSilence(ctx, store.Silence{
			ScopeKind: store.ScopeGlobal,
			Reason:    "kill switch",
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create silence: %v", err)
		}
		page := getDashboardHTML(t, h)
		if !strings.Contains(page, "PAUSED") {
			t.Errorf("dashboard missing PAUSED banner:\n%s", bannerRegion(page))
		}
	})

	t.Run("both hold at once and both are named", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.store.CreateSilence(ctx, store.Silence{
			ScopeKind: store.ScopeGlobal, Reason: "kill", ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create silence: %v", err)
		}
		if _, err := h.store.CreateNotifyMute(ctx, store.NotifyMute{
			ScopeKind: store.ScopeGlobal, Reason: "quiet", ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("create notify mute: %v", err)
		}
		page := getDashboardHTML(t, h)
		if !strings.Contains(page, "PAUSED") || !strings.Contains(page, "MUTED") {
			t.Errorf("combined banner must name both PAUSED and MUTED:\n%s", bannerRegion(page))
		}
	})
}

// dashboard logs in and returns the rendered dashboard HTML.
func getDashboardHTML(t *testing.T, h *harness) string {
	t.Helper()
	ts := httptest.NewServer(h.server.Handler())
	t.Cleanup(ts.Close)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	return page
}

// banner extracts the banner line region for a readable failure message.
func bannerRegion(page string) string {
	i := strings.Index(page, `role="alert"`)
	if i < 0 {
		return "(no banner rendered)"
	}
	end := i + 400
	if end > len(page) {
		end = len(page)
	}
	return page[i:end]
}
