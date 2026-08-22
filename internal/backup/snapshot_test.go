package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

func openTestStore(t *testing.T, dataDir string) *store.Store {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	st, err := store.Open(filepath.Join(dataDir, RestoredDBName))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

func addTarget(t *testing.T, st *store.Store, name string) {
	t.Helper()
	if _, err := st.CreateBackupTarget(context.Background(), store.BackupTarget{
		Name: name, Kind: KindResticLocal, URI: "/tmp/" + name, Enabled: true,
	}); err != nil {
		t.Fatalf("create target %s: %v", name, err)
	}
}

func hasTarget(t *testing.T, st *store.Store, name string) bool {
	t.Helper()
	_, err := st.GetBackupTarget(context.Background(), name)
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	t.Fatalf("get target %s: %v", name, err)
	return false
}

// TestLocalSnapshotRestoreRoundTrip: a snapshot taken before a mutation, then
// restored, reverts the database to the snapshot's state.
//
// Mutation check: making LocalRestore a no-op leaves the "after" row present and
// fails the "after must be gone" assertion.
func TestLocalSnapshotRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	snapDir := filepath.Join(t.TempDir(), "snap")

	st := openTestStore(t, dataDir)
	addTarget(t, st, "before")

	if _, err := LocalSnapshot(ctx, st, snapDir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// The mutation the restore must undo.
	addTarget(t, st, "after")
	if !hasTarget(t, st, "after") {
		t.Fatal("precondition: the mutation did not take")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := LocalRestore(snapDir, dataDir); err != nil {
		t.Fatalf("restore: %v", err)
	}

	st2 := openTestStore(t, dataDir)
	defer func() { _ = st2.Close() }()
	if !hasTarget(t, st2, "before") {
		t.Error("restore lost the row present at snapshot time")
	}
	if hasTarget(t, st2, "after") {
		t.Error("restore kept a row written after the snapshot: it did not revert")
	}
}

// TestLocalRestoreRefusesCorruptSnapshot: a snapshot whose database no longer
// matches its manifest is not restored — Verify catches it.
func TestLocalRestoreRefusesCorruptSnapshot(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	snapDir := filepath.Join(t.TempDir(), "snap")

	st := openTestStore(t, dataDir)
	addTarget(t, st, "before")
	if _, err := LocalSnapshot(ctx, st, snapDir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	_ = st.Close()

	// Corrupt the snapshot database after the manifest digested it.
	if err := os.WriteFile(filepath.Join(snapDir, SnapshotName), []byte("not a database"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if err := LocalRestore(snapDir, dataDir); err == nil {
		t.Fatal("restore of a corrupt snapshot should be refused")
	}
}

// TestLocalSnapshotRefusesExistingDatabase: a snapshot dir that already holds a
// database is refused rather than half-overwritten.
func TestLocalSnapshotRefusesExistingDatabase(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	snapDir := filepath.Join(t.TempDir(), "snap")

	st := openTestStore(t, dataDir)
	defer func() { _ = st.Close() }()

	if _, err := LocalSnapshot(ctx, st, snapDir); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	if _, err := LocalSnapshot(ctx, st, snapDir); err == nil {
		t.Fatal("a second snapshot over an existing database should be refused")
	}
}
