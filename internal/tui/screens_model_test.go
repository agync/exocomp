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

// TestSourcesScreenRendersLive drives the [7] Sources surface over seeded rows
// and asserts trust posture, last-sync sha and the enabled flag all render.
func TestSourcesScreenRendersLive(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.CreateSource(ctx, store.Source{
		Name: "core-infra", Kind: store.SourceGit, URI: "ssh://git/core.git",
		Ref: "refs/heads/main", LastSyncSHA: "a1b2c3d4e5f6a7b8", Signed: true,
		AllowedSigners: "dana ssh-ed25519 AAAA", Enabled: true,
	}); err != nil {
		t.Fatalf("create signed source: %v", err)
	}
	if _, err := st.CreateSource(ctx, store.Source{
		Name: "host-media", Kind: store.SourceHostPath, URI: "/srv/media", Enabled: false,
	}); err != nil {
		t.Fatalf("create hostpath source: %v", err)
	}

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 36})
	m = update(t, m, loadSourcesCmd(st)())
	m = update(t, m, key('7'))
	if m.active != screenSources {
		t.Fatalf("[7] did not open Sources, active = %v", m.active)
	}

	view := m.View().Content
	for _, want := range []string{
		"sources · 2 registered", "core-infra", "git", "refs/heads/main",
		"a1b2c3d4e5f6", "SIGNED · SIGNERS", "host-media", "hostpath", "unsigned", "no",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("sources view missing %q, view:\n%s", want, view)
		}
	}
}

// TestPoliciesScreenRendersLive drives the [8] plan-policies surface over
// seeded rows and asserts each verdict, its expression and the enabled flag.
func TestPoliciesScreenRendersLive(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-prod-destroy", Workspace: "prod",
		Expr: "plan.resource_changes.size() > 0", Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("create deny policy: %v", err)
	}
	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "gate-core", Workspace: "core-infra",
		Expr: "true", Action: store.PolicyRequireApproval,
	}); err != nil {
		t.Fatalf("create gate policy: %v", err)
	}

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 36})
	m = update(t, m, loadPoliciesCmd(st)())
	m = update(t, m, key('8'))
	if m.active != screenPolicies {
		t.Fatalf("[8] did not open Policies, active = %v", m.active)
	}

	view := m.View().Content
	for _, want := range []string{
		"plan policies · 2", "no-prod-destroy", "prod", store.PolicyDeny,
		"plan.resource_changes.size() > 0", "gate-core", "core-infra",
		store.PolicyRequireApproval, "yes",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("policies view missing %q, view:\n%s", want, view)
		}
	}
}

// TestWorkflowsScreenRendersLiveAndDetail drives the [9] Workflows surface: the
// list shows a chain's derived state and step rail, and [enter] opens the step
// ledger with each step's status and job, [esc] closes it.
func TestWorkflowsScreenRendersLiveAndDetail(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.CreateWorkflow(ctx, "provision-aurora", []store.NewWorkflowStep{
		{StepNo: 1, Kind: "provision", IdempotencyKey: "k1"},
		{StepNo: 2, Kind: "converge", IdempotencyKey: "k2"},
	}); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	job, err := st.CreateJob(ctx, store.NewJob{Runbook: "rb", Kind: "ansible", Trigger: "workflow"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := st.StartStep(ctx, "provision-aurora", 1, job.ID); err != nil {
		t.Fatalf("start step 1: %v", err)
	}
	if err := st.CompleteStep(ctx, "provision-aurora", 1, store.StepSucceeded, ""); err != nil {
		t.Fatalf("complete step 1: %v", err)
	}

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 36})
	m = update(t, m, loadWorkflowsCmd(st)())
	m = update(t, m, key('9'))
	if m.active != screenWorkflows {
		t.Fatalf("[9] did not open Workflows, active = %v", m.active)
	}
	list := m.View().Content
	for _, want := range []string{"workflows · 1 recorded", "provision-aurora", "running", "1/2"} {
		if !strings.Contains(list, want) {
			t.Errorf("workflows list missing %q, view:\n%s", want, list)
		}
	}

	// enter opens the step ledger; it shows each step's kind, status and job.
	m = update(t, m, specialKey(tea.KeyEnter))
	if m.workflowDetail != "provision-aurora" {
		t.Fatalf("enter did not open the ledger, detail = %q", m.workflowDetail)
	}
	detail := m.View().Content
	for _, want := range []string{"workflow · provision-aurora", "provision", "succeeded", "converge", "pending", "#" + itoa64(job.ID)} {
		if !strings.Contains(detail, want) {
			t.Errorf("workflow detail missing %q, view:\n%s", want, detail)
		}
	}

	// esc closes the ledger back to the list, not out of the screen.
	m = update(t, m, specialKey(tea.KeyEscape))
	if m.workflowDetail != "" || m.active != screenWorkflows {
		t.Fatalf("esc did not close the ledger: detail=%q active=%v", m.workflowDetail, m.active)
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
