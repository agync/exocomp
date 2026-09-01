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

// TestHomeSelfObservabilityRendersLive proves the Home SELF section carries the
// running version and the Litestream replication RPO age. With no replica
// configured the RPO reads "unknown · no replica configured" — never a
// fresh-looking zero.
func TestHomeSelfObservabilityRendersLive(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// A backup target keeps Home populated so the SELF section renders: the
	// first-day empty block replaces the whole body, SELF included.
	if _, err := st.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "restic-local", Kind: "restic_local", URI: "/var/lib/exocomp/restic", Enabled: true,
	}); err != nil {
		t.Fatalf("create backup target: %v", err)
	}

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadGlanceCmd(st)())
	m = update(t, m, loadBackupsCmd(st)())

	view := m.View().Content
	for _, want := range []string{"SELF", "version", "litestream", "RPO unknown", "no replica configured"} {
		if !strings.Contains(view, want) {
			t.Errorf("Home SELF section missing %q, view:\n%s", want, view)
		}
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
		Name: "host-media", Kind: store.SourcePath, URI: "/srv/media", Enabled: false,
	}); err != nil {
		t.Fatalf("create path source: %v", err)
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
		"a1b2c3d4e5f6", "SIGNED · SIGNERS", "host-media", "path", "unsigned", "no",
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

// TestSecretsScreenRendersLive drives the [s] Secrets surface over seeded rows:
// a generated secret, a user secret, an expired one and a bound one. It asserts
// the source labels, the expiry staleness markers and the (binding -> env var)
// breakdown all render — and that the sealed material (nonce, ciphertext) never
// reaches the view (invariant 4).
func TestSecretsScreenRendersLive(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// A generated secret, no expiry, and injected by a run-secret binding.
	if _, err := st.CreateSecret(ctx, store.SecretRow{
		Name: "restic-password", Kind: "backup", Source: store.SecretSourceGenerated,
		Nonce: []byte("NONCEBYTES"), Ciphertext: []byte("SEALEDCIPHERTEXT"), AAD: "restic-password|backup",
	}); err != nil {
		t.Fatalf("create generated secret: %v", err)
	}
	if _, err := st.AddRunSecret(ctx, store.RunSecret{
		Binding: "nightly-db", EnvName: "RESTIC_PASSWORD", SecretName: "restic-password",
	}); err != nil {
		t.Fatalf("bind run secret: %v", err)
	}

	// A user secret already past its expiry.
	expired := now.Add(-24 * time.Hour)
	if _, err := st.CreateSecret(ctx, store.SecretRow{
		Name: "deploy-token", Kind: "git", Source: store.SecretSourceUser, ExpiresAt: &expired,
		Nonce: []byte("NONCEBYTES"), Ciphertext: []byte("SEALEDCIPHERTEXT"), AAD: "deploy-token|git",
	}); err != nil {
		t.Fatalf("create expired secret: %v", err)
	}

	// A user secret with a future expiry, bound to nothing.
	future := now.Add(30 * 24 * time.Hour)
	if _, err := st.CreateSecret(ctx, store.SecretRow{
		Name: "smtp-relay", Kind: "notify", Source: store.SecretSourceUser, ExpiresAt: &future,
		Nonce: []byte("NONCEBYTES"), Ciphertext: []byte("SEALEDCIPHERTEXT"), AAD: "smtp-relay|notify",
	}); err != nil {
		t.Fatalf("create future secret: %v", err)
	}

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 36})
	m = update(t, m, loadSecretsCmd(st)())
	m = update(t, m, key('s'))
	if m.active != screenSecrets {
		t.Fatalf("[s] did not open Secrets, active = %v", m.active)
	}

	view := m.View().Content
	for _, want := range []string{
		"secrets · 3 stored",
		"restic-password", "generated", "no expiry",
		"used by: nightly-db as $RESTIC_PASSWORD",
		"deploy-token", "user", "EXPIRED",
		"smtp-relay", "expires in", "unbound",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("secrets view missing %q, view:\n%s", want, view)
		}
	}
	// The sealed material must never reach the view.
	for _, sealed := range []string{"SEALEDCIPHERTEXT", "NONCEBYTES"} {
		if strings.Contains(view, sealed) {
			t.Fatalf("secrets view leaked sealed material %q:\n%s", sealed, view)
		}
	}
}

// TestRunnersScreenRendersLive drives the [r] runner registry surface over
// seeded rows: a cached runner with packages and resource caps, and an
// on-the-fly runner built from a Containerfile. It asserts the columns render
// and that the bootstrap body never reaches the view (no bootstrap body,
// matching invariant 4's "never carried into the model" precedent for secrets).
func TestRunnersScreenRendersLive(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	cached, err := st.CreateRunner(ctx, store.Runner{
		Name: "fedora-ee", Mode: store.RunnerModeCached,
		Base:        "registry.fedoraproject.org/fedora-minimal:44",
		Bootstrap:   "#!/bin/sh\ndnf install -y sshpass rsync\n",
		Packages:    []string{"sshpass", "rsync"},
		AnsibleCore: "2.18.1", MemoryMax: "2G", CPUQuota: "200%",
	})
	if err != nil {
		t.Fatalf("create cached runner: %v", err)
	}
	onthefly, err := st.CreateRunner(ctx, store.Runner{
		Name: "custom-build", Mode: store.RunnerModeOnTheFly,
		Base: "docker.io/library/debian:bookworm-slim", Containerfile: "images/custom/Containerfile",
	})
	if err != nil {
		t.Fatalf("create onthefly runner: %v", err)
	}

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 36})
	m = update(t, m, loadRunnersCmd(st)())
	m = update(t, m, key('r'))
	if m.active != screenRunners {
		t.Fatalf("[r] did not open Runners, active = %v", m.active)
	}

	view := m.View().Content
	for _, want := range []string{
		"runners · 2 registered",
		"fedora-ee", store.RunnerModeCached, "registry.fedoraproject.org/fedora", "2", "2.18.1",
		cached.ContentHash[:12], cached.CreatedAt.Local().Format("2006-01-02"),
		"custom-build", store.RunnerModeOnTheFly, "docker.io/library/debian:bookworm", "0", "-",
		onthefly.ContentHash[:12], onthefly.CreatedAt.Local().Format("2006-01-02"),
	} {
		if !strings.Contains(view, want) {
			t.Errorf("runners view missing %q, view:\n%s", want, view)
		}
	}
	// The bootstrap body must never reach the view.
	if strings.Contains(view, "dnf install") {
		t.Fatalf("runners view leaked the bootstrap body:\n%s", view)
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

// TestHomeEmptyStateRendersFirstDayBlock proves the first-day Home names the
// three add commands verbatim when nothing is configured yet (copy.txt:65-66).
func TestHomeEmptyStateRendersFirstDayBlock(t *testing.T) {
	m := New(openTestStore(t))
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View().Content
	for _, want := range []string{
		"nothing configured yet",
		"exocomp backup add",
		"exocomp workspace add ./infra",
		"exocomp schedule add",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("empty Home missing %q, view:\n%s", want, view)
		}
	}
}

// TestApprovalsEmptyStateRendersNothingWaiting proves the approvals surface
// reads "nothing waiting on you" when no approval is pending (copy.txt:73).
func TestApprovalsEmptyStateRendersNothingWaiting(t *testing.T) {
	m := New(openTestStore(t))
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('5'))

	if v := m.View().Content; !strings.Contains(v, "nothing waiting on you") {
		t.Fatalf("empty approvals missing the good state, view:\n%s", v)
	}
}

// TestEventsEmptyStateRendersIngestHint proves the events surface points at
// the ingest command when no event has landed yet (copy.txt:74).
func TestEventsEmptyStateRendersIngestHint(t *testing.T) {
	m := New(openTestStore(t))
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 36})
	m = update(t, m, key('6'))

	if v := m.View().Content; !strings.Contains(v, "no events yet — point your alert source at exocomp ingest") {
		t.Fatalf("empty events missing the ingest hint, view:\n%s", v)
	}
}
