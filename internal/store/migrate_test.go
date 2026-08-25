package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestConcurrentMigrationAppliesOnce: the CLI and the daemon both open the
// same file, and an upgraded binary finds it behind. Which migrations to run
// is decided before any write lock exists, so the transaction that applies one
// has to re-check the version it was chosen against — two openers otherwise
// both run 0002 and the second fails on a table that already exists.
//
// The two opens race SQLite's real write lock, which needs real wall-clock
// time to hand off. On a machine also running every other package's -race
// binary at once (make check), scheduling delays can eat into that time far
// past what the production busy_timeout budgets for two operator-scale
// processes (docs/operations/serve.md's known limit is this same class of
// race). The longer timeout here is headroom against that unrelated
// contention; every opener still races the same store.go migrate() through
// the same recheck-inside-tx path the production 5s default uses.
func TestConcurrentMigrationAppliesOnce(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigration1(t, path)
	raceDSN := dsnWithBusyTimeout(path, 30*time.Second)

	const openers = 2 // the CLI and the daemon, the scenario above
	errs := make(chan error, openers)
	for range openers {
		go func() {
			s, err := openDSN(path, raceDSN)
			if err != nil {
				errs <- err
				return
			}
			errs <- s.Close()
		}()
	}
	for range openers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent open: %v", err)
		}
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.CreateJob(context.Background(),
		NewJob{Runbook: "after", Kind: "shell", Trigger: "manual"}); err != nil {
		t.Errorf("the migrated database is unusable: %v", err)
	}
}

// TestMigration0027CollapsesSourceKinds pins a database just before 0027, seeds
// the three legacy kinds plus inventories referencing them, then opens with the
// current binary and confirms 0027 mapped hostpath->path/read-only and
// volpath->path/writable, left git untouched, and preserved every inventory
// (the table rebuild must not let the ON DELETE CASCADE drop them).
func TestMigration0027CollapsesSourceKinds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")

	pinBeforeMigration(t, path, 27)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	seed := `
INSERT INTO source (name, kind, uri, signed, allowed_signers, enabled, created_at)
VALUES ('host', 'hostpath', '/srv/host', 0, '', 1, 0),
       ('vol',  'volpath',  '/data/vol', 0, '', 1, 0),
       ('repo', 'git',      'git@f:o/r.git', 0, '', 1, 0);
INSERT INTO inventory (name, source_id, path, kind, env_binding_json, created_at)
VALUES ('host-inv', (SELECT id FROM source WHERE name='host'), 'hosts.ini', 'static', '{}', 0),
       ('vol-inv',  (SELECT id FROM source WHERE name='vol'),  'hosts.ini', 'static', '{}', 0),
       ('free-inv', NULL, 'hosts.ini', 'static', '{}', 0);
`
	if _, err := raw.ExecContext(ctx, seed); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open for 0027 upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	want := map[string]struct {
		kind     string
		writable bool
	}{
		"host": {SourcePath, false},
		"vol":  {SourcePath, true},
		"repo": {SourceGit, false},
	}
	for name, w := range want {
		got, err := s.SourceByName(ctx, name)
		if err != nil {
			t.Fatalf("source %q after 0027: %v", name, err)
		}
		if got.Kind != w.kind || got.Writable != w.writable {
			t.Fatalf("source %q = kind %q writable %v, want kind %q writable %v",
				name, got.Kind, got.Writable, w.kind, w.writable)
		}
	}

	invs, err := s.ListInventories(ctx)
	if err != nil {
		t.Fatalf("list inventories after 0027: %v", err)
	}
	if len(invs) != 3 {
		t.Fatalf("0027 lost inventories: have %d, want 3 (the cascade must not fire): %+v", len(invs), invs)
	}
}

// pinBeforeMigration writes a database holding every embedded migration below
// version, leaving that version and everything after it for a later Open to
// apply.
func pinBeforeMigration(t *testing.T, path string, version int) {
	t.Helper()
	ctx := context.Background()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	applied := 0
	for _, m := range migrations {
		if m.version >= version {
			break
		}
		if _, err := raw.ExecContext(ctx, m.body); err != nil {
			t.Fatalf("apply migration %s: %v", m.name, err)
		}
		applied = m.version
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(applied)); err != nil {
		t.Fatalf("set user_version to %d: %v", applied, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
}

// pinAtMigration1 writes a database holding migration 0001 only, the state an
// installation upgraded from an older binary is in.
func pinAtMigration1(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	var v1 migration
	for _, m := range migrations {
		if m.version == 1 {
			v1 = m
			break
		}
	}
	if v1.version != 1 {
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
}
