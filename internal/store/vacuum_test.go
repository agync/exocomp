package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVacuumIntoRefusesExistingDestination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, err := Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(dest, []byte("existing"), 0o600); err != nil {
		t.Fatalf("seed destination: %v", err)
	}

	if err := s.VacuumInto(ctx, dest); err == nil {
		t.Fatal("VacuumInto succeeded against an existing destination, want error")
	}
}

// TestVacuumIntoSucceedsDuringWriterTransaction pins the store's
// single-connection stall: because Open sets SetMaxOpenConns(1), a
// VacuumInto call started while another goroutine holds an open write
// transaction on the same *Store must block until that transaction
// concludes, then succeed rather than error.
func TestVacuumIntoSucceedsDuringWriterTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, err := Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin writer tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job (runbook, kind, "trigger", state, queued_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"mid-tx", "shell", "manual", string(StateQueued), time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert inside writer tx: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "snapshot.db")
	done := make(chan error, 1)
	go func() { done <- s.VacuumInto(ctx, dest) }()

	select {
	case err := <-done:
		t.Fatalf("VacuumInto returned (err=%v) before the writer transaction committed", err)
	case <-time.After(200 * time.Millisecond):
		// Expected: VacuumInto is still waiting on the store's one pooled
		// connection, held by the open writer transaction.
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit writer tx: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("VacuumInto after writer commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("VacuumInto did not return after the writer transaction committed")
	}

	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
}
