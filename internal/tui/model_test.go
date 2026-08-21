package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

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
	m := New(openTestStore(t))
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	if got := m.View().Content; !strings.Contains(got, "no jobs yet") {
		t.Fatalf("initial screen not Jobs, view:\n%s", got)
	}

	m = update(t, m, key('2'))
	if m.active != screenBackups {
		t.Fatalf("active = %v, want screenBackups", m.active)
	}
	if got := m.View().Content; !strings.Contains(got, "no backup targets configured") {
		t.Fatalf("did not switch to Backups, view:\n%s", got)
	}

	m = update(t, m, specialKey(tea.KeyTab))
	if m.active != screenJobs {
		t.Fatalf("tab did not switch back to Jobs, active = %v", m.active)
	}

	m = update(t, m, key('2'))
	m = update(t, m, key('1'))
	if m.active != screenJobs {
		t.Fatalf("'1' did not select Jobs, active = %v", m.active)
	}
}

func TestQuitSendsQuitCmd(t *testing.T) {
	m := New(openTestStore(t))
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

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

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

	m := New(st)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
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
	m := New(st)
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
	m := New(openTestStore(t))
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = update(t, m, key('x'))
	if m.active != screenJobs || m.tailing {
		t.Fatalf("unrecognized key changed state: active=%v tailing=%v", m.active, m.tailing)
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
