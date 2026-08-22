package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/store"
)

// TestKeymapRootAndWalkBack pins the settled keymap deltas: [0] is Home and the
// root, q quits only there, esc walks a sub-screen back to Home, and [5]/[6]
// reach the new screens.
func TestKeymapRootAndWalkBack(t *testing.T) {
	m := New(openTestStore(t))
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m = update(t, m, key('5'))
	if m.active != screenApprovals {
		t.Fatalf("[5] did not open Approvals, active = %v", m.active)
	}
	// q is inert away from the root.
	next, cmd := m.Update(key('q'))
	m = next.(Model)
	if m.quitting || cmd != nil {
		t.Fatal("q quit from a sub-screen; it must quit only at the root")
	}
	// esc walks back to Home.
	m = update(t, m, specialKey(tea.KeyEscape))
	if m.active != screenHome {
		t.Fatalf("esc did not walk back to Home, active = %v", m.active)
	}
	// q at the root quits.
	next, cmd = m.Update(key('q'))
	m = next.(Model)
	if !m.quitting || cmd == nil {
		t.Fatal("q at the root did not quit")
	}

	m2 := update(t, New(openTestStore(t)), tea.WindowSizeMsg{Width: 80, Height: 24})
	m2 = update(t, m2, key('6'))
	if m2.active != screenEvents {
		t.Fatalf("[6] did not open Events, active = %v", m2.active)
	}
}

// TestBannerShowsMutedDistinctFromPaused proves the split is visible on the
// live Home: a notify mute renders MUTED, a global silence renders PAUSED, and
// with both the banner names both.
func TestBannerShowsMutedDistinctFromPaused(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := st.CreateNotifyMute(ctx, store.NotifyMute{
		ScopeKind: store.ScopeGlobal, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create mute: %v", err)
	}

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadGlanceCmd(st)())

	view := m.View().Content
	if !strings.Contains(view, "MUTED") {
		t.Fatalf("Home banner missing MUTED with an active mute:\n%s", view)
	}
	if strings.Contains(view, "PAUSED") {
		t.Fatalf("Home banner shows PAUSED with only a mute active:\n%s", view)
	}

	if _, err := st.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeGlobal, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("create silence: %v", err)
	}
	m = update(t, m, loadGlanceCmd(st)())
	if view := m.View().Content; !strings.Contains(view, "PAUSED") || !strings.Contains(view, "MUTED") {
		t.Fatalf("Home banner did not name both holds:\n%s", view)
	}
}

// TestApprovalsAndEventsRenderLive drives the two new screens over seeded rows
// and asserts their live assembly reaches the fixture renderers.
func TestApprovalsAndEventsRenderLive(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	job, err := st.CreateJob(ctx, store.NewJob{Runbook: "rb", Kind: "tofu", Trigger: "reactive"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := st.CreateApproval(ctx, store.Approval{
		JobID: job.ID, Workspace: "core-infra", RequestedAt: now.Add(-2 * time.Minute),
		ExpiresAt: now.Add(time.Hour), TokenHash: "h", State: store.ApprovalPending,
	}); err != nil {
		t.Fatalf("create approval: %v", err)
	}

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 36})
	m = update(t, m, loadGlanceCmd(st)())

	m = update(t, m, key('5'))
	if v := m.View().Content; !strings.Contains(v, "approvals · 1 waiting") || !strings.Contains(v, "core-infra") {
		t.Fatalf("approvals screen missing live data:\n%s", v)
	}

	m = update(t, m, key('6'))
	if v := m.View().Content; !strings.Contains(v, "events ·") {
		t.Fatalf("events screen missing summary:\n%s", v)
	}
}
