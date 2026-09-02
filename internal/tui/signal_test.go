package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// seedBackupRun registers a target and runs one run to a given state, finishing
// at the given time, so the dead-man tests control backup history exactly.
func seedBackupRun(t *testing.T, st *store.Store, state string, finishedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	target, err := st.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "t" + finishedAt.Format("150405"), Kind: "restic_local", URI: "/srv/x", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	run, err := st.StartBackupRun(ctx, target.ID)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run.State = state
	run.FinishedAt = &finishedAt
	if state == store.BackupSucceeded {
		run.SnapshotID = "snap"
	}
	if err := st.FinishBackupRun(ctx, run); err != nil {
		t.Fatalf("finish run: %v", err)
	}
}

// TestSignalLineDeadManAge proves the footer's dead-man segment reports the
// newest SUCCEEDED backup run's age from the store — a failed run finishing
// later must not mask the older good backup — and "--" honestly when none has
// ever succeeded. The age is resolved on the glance load path, never in View.
func TestSignalLineDeadManAge(t *testing.T) {
	st := openTestStore(t)
	good := time.Now().Add(-25 * time.Minute).UTC()
	seedBackupRun(t, st, store.BackupSucceeded, good)
	bad := time.Now().Add(-5 * time.Minute).UTC()
	seedBackupRun(t, st, store.BackupFailed, bad)

	m := New(st, nil)
	m = update(t, m, loadGlanceCmd(st)())
	got := m.signalLine()
	if !strings.Contains(got, "dead-man 25m") || !strings.Contains(got, "notify loud") {
		t.Errorf("signalLine = %q, want the last good backup's age and loud notify", got)
	}

	none := New(openTestStore(t), nil)
	seedBackupRun(t, none.store, store.BackupFailed, bad)
	none = update(t, none, loadGlanceCmd(none.store)())
	if got := none.signalLine(); !strings.Contains(got, "dead-man --") {
		t.Errorf("signalLine with no succeeded run = %q, want dead-man --", got)
	}
}
