package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobEventDeleteRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, err := Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	job, err := s.CreateJob(ctx, NewJob{Runbook: "keep", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := s.AppendJobEvent(ctx, JobEvent{JobID: job.ID, Msg: "hello"}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	_, err = s.db.ExecContext(ctx, `DELETE FROM job_event WHERE job_id = ?`, job.ID)
	if err == nil {
		t.Fatal("delete on job_event succeeded, want append-only rejection")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("delete error = %v, want append-only rejection", err)
	}
}

// TestMigration0001To0002UpgradesExistingDB starts a database pinned at
// migration 0001 (no DELETE guard) and confirms that opening it with the
// current binary applies 0002 forward and the DELETE guard takes effect,
// without touching any data migration 0001 already wrote.
func TestMigration0001To0002UpgradesExistingDB(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	var v1 migration
	found := false
	for _, m := range migrations {
		if m.version == 1 {
			v1, found = m, true
			break
		}
	}
	if !found {
		t.Fatal("migration 0001 not found among embedded migrations")
	}

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := raw.ExecContext(ctx, v1.body); err != nil {
		t.Fatalf("apply migration 0001: %v", err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("set user_version to 1: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at v1 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 2 {
		t.Fatalf("schema version = %d after open, want >= 2 (0002 applied)", version)
	}

	job, err := s.CreateJob(ctx, NewJob{Runbook: "keep", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job on upgraded db: %v", err)
	}
	if _, err := s.AppendJobEvent(ctx, JobEvent{JobID: job.ID, Msg: "hello"}); err != nil {
		t.Fatalf("append event on upgraded db: %v", err)
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM job_event WHERE job_id = ?`, job.ID)
	if err == nil {
		t.Fatal("delete on job_event succeeded after 0001->0002 upgrade, want rejection")
	}
}
