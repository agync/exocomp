package store_test

import (
	"context"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// seedDeadManRun registers a target and runs one run to a given state,
// finishing at the given time, so the dead-man tests control history exactly.
func seedDeadManRun(t *testing.T, s *store.Store, state string, finishedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	target, err := s.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "t" + finishedAt.Format("150405"), Kind: "restic_local", URI: "/srv/x", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	run, err := s.StartBackupRun(ctx, target.ID)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run.State = state
	run.FinishedAt = &finishedAt
	if state == store.BackupSucceeded {
		run.SnapshotID = "snap"
	}
	if err := s.FinishBackupRun(ctx, run); err != nil {
		t.Fatalf("finish run: %v", err)
	}
}

// TestLastSucceededBackupTime is the GATE for the ONE dead-man rule: the newest
// SUCCEEDED run wins even when a FAILED run finished later (a failed newest run
// must never mask an older good backup), and nothing-succeeded reports false.
func TestLastSucceededBackupTime(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	// A recent failure AFTER an older success: the rule must report the older
	// success's age, not "never".
	good := time.Now().Add(-3 * time.Hour).UTC()
	seedDeadManRun(t, s, store.BackupSucceeded, good)
	bad := time.Now().Add(-30 * time.Minute).UTC()
	seedDeadManRun(t, s, store.BackupFailed, bad)

	when, ok := s.LastSucceededBackupTime(ctx)
	if !ok {
		t.Fatal("LastSucceededBackupTime reported no success though one exists")
	}
	if when.UnixMilli() != good.UnixMilli() {
		t.Fatalf("LastSucceededBackupTime = %v, want the succeeded run %v", when, good)
	}

	// Nothing succeeded → false.
	empty, _ := open(t)
	if _, ok := empty.LastSucceededBackupTime(ctx); ok {
		t.Fatal("an empty store must report no succeeded backup")
	}
}

// TestNewestSucceededBackupRun pins the row behind the ONE rule: the newest
// SUCCEEDED run returns with its target name, a newer FAILED run never takes
// its place, and an empty store reports ok=false.
func TestNewestSucceededBackupRun(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	good := time.Now().Add(-3 * time.Hour).UTC()
	seedDeadManRun(t, s, store.BackupSucceeded, good)
	bad := time.Now().Add(-30 * time.Minute).UTC()
	seedDeadManRun(t, s, store.BackupFailed, bad)

	run, target, ok, err := s.NewestSucceededBackupRun(ctx)
	if err != nil {
		t.Fatalf("NewestSucceededBackupRun: %v", err)
	}
	if !ok {
		t.Fatal("NewestSucceededBackupRun reported no success though one exists")
	}
	if run.FinishedAt == nil || run.FinishedAt.UnixMilli() != good.UnixMilli() {
		t.Fatalf("NewestSucceededBackupRun = finished %v, want the succeeded run %v", run.FinishedAt, good)
	}
	if run.State != store.BackupSucceeded {
		t.Fatalf("NewestSucceededBackupRun state = %q, want succeeded", run.State)
	}
	if target.Name == "" {
		t.Fatal("NewestSucceededBackupRun returned no target name")
	}

	empty, _ := open(t)
	if _, _, ok, err := empty.NewestSucceededBackupRun(ctx); err != nil || ok {
		t.Fatalf("empty store = ok %v, err %v; want no success and no error", ok, err)
	}
}
