package tui

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func key(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

func specialKey(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

func shiftTabKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift} }

func update(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", next)
	}
	return got
}

func TestScreenSwitching(t *testing.T) {
	m := New(openTestStore(t), nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, loadJobsCmd(m.store)())
	m = update(t, m, loadBackupsCmd(m.store)())
	m = update(t, m, loadWorkspacesCmd(m.store)())
	m = update(t, m, loadSchedulesCmd(m.store)())

	// [0] Home is the new root; [1] selects Jobs.
	if m.active != screenHome {
		t.Fatalf("initial screen = %v, want screenHome", m.active)
	}
	m = update(t, m, key('1'))
	if got := m.View().Content; !strings.Contains(got, "no jobs yet — they appear when a schedule or alert fires") {
		t.Fatalf("[1] did not show Jobs, view:\n%s", got)
	}

	m = update(t, m, key('2'))
	if m.active != screenBackups {
		t.Fatalf("active = %v, want screenBackups", m.active)
	}
	if got := m.View().Content; !strings.Contains(got, "no backups yet — exocomp backup add") {
		t.Fatalf("did not switch to Backups, view:\n%s", got)
	}

	m = update(t, m, key('3'))
	if m.active != screenWorkspaces {
		t.Fatalf("active = %v, want screenWorkspaces", m.active)
	}
	if got := m.View().Content; !strings.Contains(got, "no workspaces yet") || !strings.Contains(got, "exocomp workspace add ./infra") {
		t.Fatalf("did not switch to Workspaces, view:\n%s", got)
	}

	m = update(t, m, key('4'))
	if m.active != screenSchedules {
		t.Fatalf("active = %v, want screenSchedules", m.active)
	}
	if got := m.View().Content; !strings.Contains(got, "no schedules yet — exocomp schedule add") {
		t.Fatalf("did not switch to Schedules, view:\n%s", got)
	}

	// tab cycles every screen in order and wraps at both ends.
	m = update(t, m, key('1'))
	for _, want := range []screen{
		screenBackups, screenWorkspaces, screenSchedules, screenApprovals,
		screenEvents, screenSources, screenPolicies, screenWorkflows,
		screenSecrets, screenRunners, screenHome, screenJobs,
	} {
		m = update(t, m, specialKey(tea.KeyTab))
		if m.active != want {
			t.Fatalf("tab did not advance to %v, active = %v", want, m.active)
		}
	}
	// shift-tab walks the same order backwards.
	for _, want := range []screen{
		screenHome, screenRunners, screenSecrets, screenWorkflows,
		screenPolicies, screenSources, screenEvents, screenApprovals,
		screenSchedules, screenWorkspaces, screenBackups, screenJobs,
	} {
		m = update(t, m, shiftTabKey())
		if m.active != want {
			t.Fatalf("shift-tab did not step back to %v, active = %v", want, m.active)
		}
	}

	m = update(t, m, key('2'))
	m = update(t, m, key('1'))
	if m.active != screenJobs {
		t.Fatalf("'1' did not select Jobs, active = %v", m.active)
	}
}

func TestQuitSendsQuitCmd(t *testing.T) {
	m := New(openTestStore(t), nil)
	next, cmd := m.Update(key('q'))
	got := next.(Model)
	if !got.quitting {
		t.Fatal("quitting flag not set after 'q'")
	}
	if cmd == nil {
		t.Fatal("expected a quit command, got nil")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("cmd() = %T, want tea.QuitMsg", cmd())
	}
}

func seedJob(t *testing.T, st *store.Store, kind string, final store.JobState, exit *int) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := st.CreateJob(ctx, store.NewJob{Runbook: "rb", Kind: kind, Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if final == store.StateQueued {
		return job
	}
	if err := st.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("transition to running: %v", err)
	}
	if final != store.StateRunning {
		if err := st.TransitionJob(ctx, job.ID, store.StateRunning, final); err != nil {
			t.Fatalf("transition to %s: %v", final, err)
		}
	}
	if exit != nil {
		if err := st.SetJobExitCode(ctx, job.ID, *exit); err != nil {
			t.Fatalf("set exit code: %v", err)
		}
	}
	job, err = st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return job
}

func TestSelectionMovementAndTailToggling(t *testing.T) {
	st := openTestStore(t)
	zero := 0
	first := seedJob(t, st, "ansible", store.StateSucceeded, &zero)
	second := seedJob(t, st, "restic", store.StateRunning, nil)
	_ = second

	if _, err := st.AppendJobEvent(context.Background(), store.JobEvent{
		JobID: first.ID, Level: store.LevelInfo, Msg: "starting backup leg",
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('1')) // Home is the root; move to Jobs to drive its table

	msg := loadJobsCmd(st)()
	m = update(t, m, msg)

	// loadJobsCmd lists newest-queued first, so the freshly-created "second"
	// job (restic, running) sits at cursor 0 and "first" (ansible,
	// succeeded) at cursor 1.
	if m.jobsTable.model.Cursor() != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.jobsTable.model.Cursor())
	}

	m = update(t, m, specialKey(tea.KeyDown))
	if m.jobsTable.model.Cursor() != 1 {
		t.Fatalf("cursor after down = %d, want 1", m.jobsTable.model.Cursor())
	}

	m = update(t, m, specialKey(tea.KeyEnter))
	if !m.tailing {
		t.Fatal("enter did not start tailing")
	}
	if m.tailJobID != first.ID {
		t.Fatalf("tailing job %d, want %d (selected row)", m.tailJobID, first.ID)
	}

	m = update(t, m, loadEventsCmd(st, first.ID)())
	if got := m.View().Content; !strings.Contains(got, "starting backup leg") {
		t.Fatalf("tail view missing event message, view:\n%s", got)
	}

	m = update(t, m, specialKey(tea.KeyEscape))
	if m.tailing {
		t.Fatal("esc did not stop tailing")
	}
	if got := m.View().Content; !strings.Contains(got, "ansible") || !strings.Contains(got, "restic") {
		t.Fatalf("jobs screen missing seeded rows after esc, view:\n%s", got)
	}
}

func TestStoreBackedViewRendersBothScreens(t *testing.T) {
	st := openTestStore(t)
	zero, thirteen := 0, 13
	seedJob(t, st, "ansible", store.StateSucceeded, &zero)
	seedJob(t, st, "restic", store.StateFailed, &thirteen)
	seedJob(t, st, "git-bundle", store.StateRunning, nil)

	ctx := context.Background()
	local, err := st.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "restic-local", Kind: "restic_local", URI: "/var/lib/exocomp/restic", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create backup target: %v", err)
	}
	run, err := st.StartBackupRun(ctx, local.ID)
	if err != nil {
		t.Fatalf("start backup run: %v", err)
	}
	run.State = store.BackupSucceeded
	run.SnapshotID = "a1b2c3d4"
	run.BytesAdded = 5 * 1024 * 1024
	if err := st.FinishBackupRun(ctx, run); err != nil {
		t.Fatalf("finish backup run: %v", err)
	}

	if _, err := st.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "git-remote", Kind: "git", URI: "ssh://git.example/backups.git", Enabled: true,
	}); err != nil {
		t.Fatalf("create second backup target: %v", err)
	}

	if _, err := st.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "restic-paused", Kind: "restic_local", URI: "/var/lib/exocomp/paused", Enabled: false,
	}); err != nil {
		t.Fatalf("create disabled backup target: %v", err)
	}

	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = update(t, m, key('1')) // leave Home for the Jobs table
	m = update(t, m, loadJobsCmd(st)())
	m = update(t, m, loadBackupsCmd(st)())

	jobsView := m.View().Content
	for _, want := range []string{"ansible", "succeeded", "restic", "failed", "13", "git-bundle", "running"} {
		if !strings.Contains(jobsView, want) {
			t.Errorf("jobs view missing %q, view:\n%s", want, jobsView)
		}
	}

	m = update(t, m, key('2'))
	backupsView := m.View().Content
	for _, want := range []string{"restic-local", "restic_local", "succeeded", "a1b2c3d4", "5.0MiB", "git-remote", "git", "-", "restic-paused", "disabled"} {
		if !strings.Contains(backupsView, want) {
			t.Errorf("backups view missing %q, view:\n%s", want, backupsView)
		}
	}
}

func TestTickReloadsActiveScreen(t *testing.T) {
	st := openTestStore(t)
	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	cmd := m.refreshCmd()
	if cmd == nil {
		t.Fatal("refreshCmd returned nil")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("refreshCmd() = %T, want tea.BatchMsg", msg)
	}
	if len(batch) < 2 {
		t.Fatalf("batch has %d cmds, want at least 2 (tick + jobs load)", len(batch))
	}
}

func TestNoOpKeysLeaveScreenUnchanged(t *testing.T) {
	m := New(openTestStore(t), nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('x'))
	if m.active != screenHome || m.tailing {
		t.Fatalf("unrecognized key changed state: active=%v tailing=%v", m.active, m.tailing)
	}
}

// seedTofuJob creates a plan or apply job the way `workspace plan`/`workspace
// apply` do (cmd/exocomp/workspace.go): Runbook "workspace:<name>", Kind
// executor.KindTofu, and the concurrency key the Workspaces screen groups
// jobs by.
func seedTofuJob(t *testing.T, st *store.Store, ws store.Workspace, op string, final store.JobState) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := st.CreateJob(ctx, store.NewJob{
		Runbook:        "workspace:" + ws.Name,
		Kind:           executor.KindTofu,
		Trigger:        "manual",
		Args:           store.JobArgs{Argv: []string{"tofu", op}},
		ConcurrencyKey: workspaceTofuConcurrencyKey(ws.Name),
	})
	if err != nil {
		t.Fatalf("create %s job: %v", op, err)
	}
	if final == store.StateQueued {
		return job
	}
	if err := st.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("transition to running: %v", err)
	}
	if final != store.StateRunning {
		if err := st.TransitionJob(ctx, job.ID, store.StateRunning, final); err != nil {
			t.Fatalf("transition to %s: %v", final, err)
		}
	}
	job, err = st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return job
}

// appendPlanOutcomeEvent appends the event `tofu plan` itself writes
// (executor.reportPlanOutcome): raw_json shaped {"tofu":{"operation":"plan",
// "changes":...}}, which executor.TofuPlanChanges and the Workspaces screen's
// drift marker both read back.
func appendPlanOutcomeEvent(t *testing.T, st *store.Store, jobID int64, changes bool) {
	t.Helper()
	raw, err := json.Marshal(map[string]executor.TofuPlanOutcome{
		"tofu": {Operation: "plan", Changes: changes},
	})
	if err != nil {
		t.Fatalf("encode plan outcome: %v", err)
	}
	if _, err := st.AppendJobEvent(context.Background(), store.JobEvent{
		JobID: jobID, Level: store.LevelSystem, Msg: "tofu plan", RawJSON: string(raw),
	}); err != nil {
		t.Fatalf("append plan outcome event: %v", err)
	}
}

func TestWorkspacesScreenRendersStatusAndDrift(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	drifted, err := st.CreateWorkspace(ctx, store.Workspace{Name: "edge", TofuDir: "/srv/tofu/edge"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := st.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: drifted.ID, Lineage: "l1", Ciphertext: []byte("ct"),
	}); err != nil {
		t.Fatalf("append tfstate: %v", err)
	}
	plan := seedTofuJob(t, st, drifted, "plan", store.StateSucceeded)
	appendPlanOutcomeEvent(t, st, plan.ID, true)
	apply := seedTofuJob(t, st, drifted, "apply", store.StateFailed)

	clean, err := st.CreateWorkspace(ctx, store.Workspace{Name: "core", TofuDir: "/srv/tofu/core"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	cleanPlan := seedTofuJob(t, st, clean, "plan", store.StateSucceeded)
	appendPlanOutcomeEvent(t, st, cleanPlan.ID, false)

	if _, err := st.CreateWorkspace(ctx, store.Workspace{
		Name: "unplanned", TofuDir: "/srv/tofu/unplanned",
	}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = update(t, m, loadWorkspacesCmd(st)())
	m = update(t, m, key('3'))

	view := m.View().Content
	for _, want := range []string{"edge", "1", "succeeded", "failed", "drift", "core", "unplanned", "never"} {
		if !strings.Contains(view, want) {
			t.Errorf("workspaces view missing %q, view:\n%s", want, view)
		}
	}

	// "core" planned clean: its row must not carry the drift marker, only
	// "edge"'s does. Count occurrences of the marker text across the whole
	// view rather than assuming column position.
	if got := strings.Count(view, "drift"); got != 1 {
		t.Errorf("drift marker appears %d times, want 1 (view):\n%s", got, view)
	}

	// ListWorkspaces orders by name, so the rows are core, edge, unplanned;
	// move the cursor down onto "edge" and tail its latest tofu job, the
	// apply that ran after the plan.
	m = update(t, m, specialKey(tea.KeyDown))
	m = update(t, m, specialKey(tea.KeyEnter))
	if !m.tailing {
		t.Fatal("enter on workspaces did not start tailing")
	}
	if m.tailJobID != apply.ID {
		t.Fatalf("tailing job %d, want %d (edge's latest job)", m.tailJobID, apply.ID)
	}
	m = update(t, m, specialKey(tea.KeyEscape))

	// A workspace with no tofu job yet has nothing to tail: Enter on it is a
	// no-op, not a crash or a tail of some other job.
	m = update(t, m, specialKey(tea.KeyDown))
	m = update(t, m, specialKey(tea.KeyEnter))
	if m.tailing {
		t.Fatal("enter on an unplanned workspace should not start tailing")
	}
}

func TestSchedulesScreenRendersEnabledAndDisabled(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	active, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly-backup", CronExpr: "@daily", Kind: store.ScheduleBackup,
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	job, err := st.CreateJob(ctx, store.NewJob{Runbook: "rb", Kind: "backup", Trigger: "cron"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := st.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("transition to running: %v", err)
	}
	if err := st.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("transition to succeeded: %v", err)
	}
	next := time.Now().Add(24 * time.Hour)
	if err := st.RecordScheduleFire(ctx, active.ID, &job.ID, &next); err != nil {
		t.Fatalf("record schedule fire: %v", err)
	}

	// A drift-check schedule: a tofu-plan whose last fire's job state is the
	// last drift outcome the screen surfaces, and whose kind must not truncate.
	drift, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "drift-core", CronExpr: "@every 6h", Kind: store.ScheduleTofuPlan,
	})
	if err != nil {
		t.Fatalf("create drift schedule: %v", err)
	}
	driftJob, err := st.CreateJob(ctx, store.NewJob{Runbook: "workspace:core", Kind: "tofu", Trigger: "cron"})
	if err != nil {
		t.Fatalf("create drift job: %v", err)
	}
	if err := st.TransitionJob(ctx, driftJob.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("transition drift to running: %v", err)
	}
	if err := st.TransitionJob(ctx, driftJob.ID, store.StateRunning, store.StateFailed); err != nil {
		t.Fatalf("transition drift to failed: %v", err)
	}
	driftNext := time.Now().Add(6 * time.Hour)
	if err := st.RecordScheduleFire(ctx, drift.ID, &driftJob.ID, &driftNext); err != nil {
		t.Fatalf("record drift fire: %v", err)
	}

	if _, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "paused-plan", CronExpr: "@every 5m", Kind: store.ScheduleShell,
	}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if err := st.SetScheduleEnabled(ctx, "paused-plan", false); err != nil {
		t.Fatalf("disable schedule: %v", err)
	}

	m := New(st, nil)
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = update(t, m, loadSchedulesCmd(st)())
	m = update(t, m, key('4'))

	view := m.View().Content
	for _, want := range []string{
		"nightly-backup", "@daily", store.ScheduleBackup, "yes", "succeeded",
		"drift-core", store.ScheduleTofuPlan, "failed", // the drift outcome, kind untruncated
		"FIRED", // the last-fire column reached parity with the web surface
		"paused-plan", "@every 5m", store.ScheduleShell, "no",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("schedules view missing %q, view:\n%s", want, view)
		}
	}
}

func TestTimeFormatting(t *testing.T) {
	zero := time.Time{}
	if got := formatTime(zero); got != emptyCell {
		t.Fatalf("formatTime(zero) = %q, want %q", got, emptyCell)
	}
	if got := formatTimePtr(nil); got != emptyCell {
		t.Fatalf("formatTimePtr(nil) = %q, want %q", got, emptyCell)
	}
}
