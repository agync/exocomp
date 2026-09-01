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

// TestMigration0030UpgradesFrom029 pins a database at 0029 and confirms that
// opening it with the current binary applies 0030 forward: check_row and
// check_state appear, and the check store's own CRUD/state round-trips on the
// upgraded schema.
func TestMigration0030UpgradesFrom029(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")

	pinBeforeMigration(t, path, 30)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var n int
	if err := raw.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('check_row', 'check_state')`).Scan(&n); err != nil {
		t.Fatalf("probe check tables at 0029: %v", err)
	}
	if n != 0 {
		t.Fatalf("check tables already exist at 0029; the pin is not honest (found %d)", n)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at 0029 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 30 {
		t.Fatalf("schema version = %d after open, want >= 30 (0030 applied)", version)
	}

	created, err := s.CreateCheck(ctx, NewCheck{
		Name: "at-home", Kind: CheckTCP, IntervalS: 300,
		Targets: []CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create check on upgraded db: %v", err)
	}
	if err := s.SetCheckState(ctx, CheckState{
		CheckID: created.ID, TargetName: "laptop", Status: CheckUp,
		Consecutive: 1, Emitted: CheckUp, FirstSeen: time.Now(), LastChange: time.Now(),
	}); err != nil {
		t.Fatalf("set state on upgraded db: %v", err)
	}
	if _, err := s.GetCheckState(ctx, created.ID, "laptop"); err != nil {
		t.Fatalf("read state on upgraded db: %v", err)
	}
}

// TestMigration0031UpgradesFrom030 pins a database at 0030 and confirms the
// rule_fire dedup_key column and its index arrive in place: a pre-0031 fire row
// defaults to ” and a fire written with a key reads back with it.
func TestMigration0031UpgradesFrom030(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")

	pinBeforeMigration(t, path, 31)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	rows, err := raw.QueryContext(ctx, `PRAGMA table_info(rule_fire)`)
	if err != nil {
		t.Fatalf("read rule_fire columns at 0030: %v", err)
	}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == "dedup_key" {
			t.Fatal("rule_fire already has dedup_key at 0030; the pin is not honest")
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close column rows: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at 0030 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 31 {
		t.Fatalf("schema version = %d after open, want >= 31 (0031 applied)", version)
	}

	var idx int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'rule_fire_rule_key'`).Scan(&idx); err != nil {
		t.Fatalf("probe index: %v", err)
	}
	if idx != 1 {
		t.Fatal("rule_fire_rule_key index missing after 0031")
	}

	rule, err := s.CreateRule(ctx, NewRule{Name: "r", CELExpr: "true", ActionKind: ActionNotify})
	if err != nil {
		t.Fatalf("create rule on upgraded db: %v", err)
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO rule_fire (rule_id, ts, job_id, dedup_key) VALUES (?, ?, NULL, ?)`,
		rule.ID, now, "check:a:b"); err != nil {
		t.Fatalf("insert keyed fire on upgraded db: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO rule_fire (rule_id, ts, job_id) VALUES (?, ?, NULL)`, rule.ID, now+1); err != nil {
		t.Fatalf("insert legacy fire on upgraded db: %v", err)
	}

	var keyed, legacy string
	if err := s.db.QueryRowContext(ctx,
		`SELECT dedup_key FROM rule_fire WHERE rule_id = ? AND ts = ?`, rule.ID, now).Scan(&keyed); err != nil {
		t.Fatalf("read keyed fire: %v", err)
	}
	if keyed != "check:a:b" {
		t.Fatalf("keyed fire read back with dedup_key %q", keyed)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT dedup_key FROM rule_fire WHERE rule_id = ? AND ts = ?`, rule.ID, now+1).Scan(&legacy); err != nil {
		t.Fatalf("read legacy fire: %v", err)
	}
	if legacy != "" {
		t.Fatalf("legacy fire read back with dedup_key %q, want ''", legacy)
	}
}

// TestMigration0032UpgradesFrom031 pins a database at 0031 and confirms the
// job_runbook index arrives in place: the runbook lookup the ListPlanArtifacts
// makes seeks through the index, and the store's plan-artifact CRUD round-trips
// on the upgraded schema.
func TestMigration0032UpgradesFrom031(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")

	pinBeforeMigration(t, path, 32)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var idx int
	if err := raw.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'job_runbook'`).Scan(&idx); err != nil {
		t.Fatalf("probe index: %v", err)
	}
	if idx != 0 {
		t.Fatal("job_runbook index already exists at 0031;the pin is not honest")
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at 0031 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 32 {
		t.Fatalf("schema version = %d after open, want >= 32 (0032 applied)", version)
	}

	// Seed enough rows that the planner has a real choice between scan and seek,
	// then pin that the runbook equality the store's plan lookup makes seeks.

	for i := range 64 {
		if _, err := s.CreateJob(ctx, NewJob{Runbook: "workspace:ws" + strconv.Itoa(i%4), Kind: "tofu", Trigger: "manual"}); err != nil {
			t.Fatalf("seed job %d: %v", i, err)
		}
	}
	planJobs := make([]int64, 0, 8)
	for i := range 8 {
		job, err := s.CreateJob(ctx, NewJob{Runbook: "workspace:web", Kind: "tofu", Trigger: "manual"})
		if err != nil {
			t.Fatalf("seed plan job %d: %v", i, err)
		}
		planJobs = append(planJobs, job.ID)
	}
	for _, id := range planJobs {
		if _, err := s.CreateArtifact(ctx, Artifact{JobID: id, Kind: "tofu.plan", Path: "/p", SHA256: "x", Size: 1}); err != nil {
			t.Fatalf("seed plan artifact %d: %v", id, err)
		}
	}

	var plan string
	if err := s.db.QueryRowContext(ctx,
		`EXPLAIN QUERY PLAN
		 SELECT a.id FROM artifact a JOIN job j ON j.id = a.job_id
		 WHERE j.runbook = 'workspace:web' AND a.kind = 'tofu.plan' LIMIT 1`).Scan(new(string), new(string), new(string), &plan); err != nil {
		t.Fatalf("explain runbook lookup: %v", err)
	}
	if !strings.Contains(plan, "job_runbook") {
		t.Fatalf("runbook lookup plan = %q, want the job_runbook index", plan)
	}

	arts, err := s.ListPlanArtifacts(ctx, "workspace:web", "tofu.plan", 3)
	if err != nil {
		t.Fatalf("list plan artifacts on upgraded db: %v", err)
	}
	if len(arts) != 3 {
		t.Fatalf("plan artifacts = %d, want 3", len(arts))
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
