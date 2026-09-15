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

// TestMigration0034UpgradesFrom33 pins the in-place jump the degraded state
// depends on: a database at 0033 (a job CHECK without 'degraded') opens with
// the current binary, the table rebuild lands, and — the rebuild's own failure
// mode — every row written at 0033 survives it: the job itself and each of the
// nine tables that reference job, whose FKs must point at the NEW job, not the
// renamed-aside legacy table (a cascade through a stale FK would have deleted
// them). The widened CHECK is then proven usable by settling a run degraded.
func TestMigration0034UpgradesFrom33(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")

	pinBeforeMigration(t, path, 34)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	seed := `
INSERT INTO job (id, runbook, kind, "trigger", state, args_json, queued_at, owner_pid, partial)
VALUES (1, 'keep', 'shell', 'manual', 'succeeded', '{}', 1, 0, 0);
INSERT INTO job_event (job_id, seq, ts, level, msg) VALUES (1, 1, 1, 'info', 'hello');
INSERT INTO artifact (id, job_id, kind, path, sha256, size) VALUES (1, 1, 'log', '/p/l', 'abc', 5);
INSERT INTO workspace (id, name, tofu_dir, auto_apply, serial) VALUES (1, 'ws', '/t', 0, 0);
INSERT INTO tfstate (id, workspace_id, serial, lineage, created_at, created_by_job_id, ciphertext)
VALUES (1, 1, 1, 'lin', 1, 1, X'00');
INSERT INTO tflock (workspace_id, lock_id, who, operation, created_at, job_id)
VALUES (1, 'L1', 'me', 'op', 1, 1);
INSERT INTO schedule (id, name, cron_expr, kind, args_json, enabled, last_job_id, next_at, last_run)
VALUES (1, 'nightly', '0 2 * * *', 'play', '{}', 1, 1, 2, 3);
INSERT INTO rule (id, name, cel_expr, action_kind) VALUES (1, 'r', 'true', 'notify');
INSERT INTO event (id, source, received_at, dedup_key, outcome, job_id)
VALUES (1, 'drift', 1, 'd1', 'fired', 1);
INSERT INTO rule_fire (id, rule_id, ts, job_id, dedup_key) VALUES (1, 1, 1, 1, 'k');
INSERT INTO approval (id, job_id, plan_artifact, workspace, plan_serial, requested_at,
                      expires_at, token_hash, state)
VALUES (1, 1, 1, 'ws', 0, 1, 2, 'tok', 'pending');
INSERT INTO workflow_step (workflow_id, step_no, kind, status, idempotency_key, job_id,
                           created_at, updated_at, require_approval)
VALUES ('wf', 1, 'play', 'succeeded', 'ik', 1, 1, 1, 0);
`
	if _, err := raw.ExecContext(ctx, seed); err != nil {
		t.Fatalf("seed 0033 rows: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at 0033 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 34 {
		t.Fatalf("schema version = %d after open, want >= 34 (0034 applied)", version)
	}

	// The FK that rode the rename-aside must now point at the NEW job, so a
	// later DELETE FROM job cascades correctly instead of following a stale
	// reference to a dropped table.
	var fkTable string
	if err := s.db.QueryRowContext(ctx,
		`SELECT [table] FROM pragma_foreign_key_list('job_event') LIMIT 1`).Scan(&fkTable); err != nil {
		t.Fatalf("read job_event FK: %v", err)
	}
	if fkTable != "job" {
		t.Fatalf("job_event FK points at %q, want job (the rebuilt table)", fkTable)
	}

	// Row survival across the rebuild: the job and one row of every table that
	// references it. A cascade through the legacy table would have deleted them.
	job, err := s.GetJob(ctx, 1)
	if err != nil {
		t.Fatalf("job row lost by 0034: %v", err)
	}
	if job.State != StateSucceeded {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	events, err := s.ListJobEvents(ctx, 1)
	if err != nil {
		t.Fatalf("job_event rows lost by 0034: %v", err)
	}
	if len(events) != 1 || events[0].Msg != "hello" {
		t.Fatalf("job_event rows = %+v, want the seeded one", events)
	}
	arts, err := s.ListArtifacts(ctx, 1)
	if err != nil {
		t.Fatalf("artifact rows lost by 0034: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(arts))
	}
	approval, err := s.ApprovalByJob(ctx, 1)
	if err != nil {
		t.Fatalf("approval rows lost by 0034: %v", err)
	}
	if approval.PlanArtifact == nil || *approval.PlanArtifact != 1 {
		t.Fatalf("approval lost its plan_artifact link: %+v", approval)
	}
	if _, err := s.LatestTFState(ctx, 1); err != nil {
		t.Fatalf("tfstate rows lost by 0034: %v", err)
	}
	if _, err := s.GetTFLock(ctx, 1); err != nil {
		t.Fatalf("tflock rows lost by 0034: %v", err)
	}
	if _, err := s.GetSchedule(ctx, "nightly"); err != nil {
		t.Fatalf("schedule rows lost by 0034: %v", err)
	}
	if _, err := s.GetEvent(ctx, 1); err != nil {
		t.Fatalf("event rows lost by 0034: %v", err)
	}
	for _, table := range []string{"rule_fire", "workflow_step"} {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("%s rows lost by 0034: have %d, want 1", table, n)
		}
	}

	// The widened CHECK admits the new state through the store's own settle
	// path — a running job degrades, and the state machine owns the move.
	created, err := s.CreateJob(ctx, NewJob{Runbook: "d", Kind: "ansible", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job on upgraded schema: %v", err)
	}
	if err := s.TransitionJob(ctx, created.ID, StateQueued, StateRunning); err != nil {
		t.Fatalf("claim on upgraded schema: %v", err)
	}
	if err := s.TransitionJob(ctx, created.ID, StateRunning, StateDegraded); err != nil {
		t.Fatalf("settle degraded on upgraded schema: %v", err)
	}
	settled, err := s.GetJob(ctx, created.ID)
	if err != nil {
		t.Fatalf("get degraded job: %v", err)
	}
	if settled.State != StateDegraded {
		t.Fatalf("settled state = %q, want degraded", settled.State)
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

// TestMigration0033UpgradesFrom032 pins the in-place upgrade the observation
// ledger depends on: a database at 0032 (no observation columns) opens with the
// current binary, gains check_state.recent_observations and
// event.check_observations_json, and both are usable — a check_state row
// carries a ring and an event carries an embedded ledger.
func TestMigration0033UpgradesFrom032(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")

	pinBeforeMigration(t, path, 33)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var cols int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('check_state') WHERE name = 'recent_observations'`).Scan(&cols); err != nil {
		t.Fatalf("probe check_state column: %v", err)
	}
	if cols != 0 {
		t.Fatal("recent_observations already exists at 0032; the pin is not honest")
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at 0032 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 33 {
		t.Fatalf("schema version = %d after open, want >= 33 (0033 applied)", version)
	}

	// A check_state row carrying the observation ring, written through the store.
	c, err := s.CreateCheck(ctx, NewCheck{
		Name: "presence", Kind: CheckTCP, IntervalS: 5,
		Targets: []CheckTarget{{Name: "phone", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	ring := `[{"observed_at":"2026-09-01T13:41:15Z","status":"up","consecutive":1},` +
		`{"observed_at":"2026-09-01T13:41:20Z","status":"up","consecutive":2}]`
	if err := s.SetCheckState(ctx, CheckState{
		CheckID: c.ID, TargetName: "phone", Status: CheckUp, Consecutive: 2,
		Emitted: CheckUp, FirstSeen: time.Unix(0, 0), LastChange: time.Unix(0, 0),
		RecentObservations: ring,
	}); err != nil {
		t.Fatalf("set check state with ring: %v", err)
	}
	st, err := s.GetCheckState(ctx, c.ID, "phone")
	if err != nil {
		t.Fatalf("get check state: %v", err)
	}
	if st.RecentObservations != ring {
		t.Fatalf("ring not persisted: got %q want %q", st.RecentObservations, ring)
	}

	// An event carrying the embedded ledger, written through the store.
	ev, err := s.CreateEvent(ctx, Event{
		Source: "check", ReceivedAt: time.Unix(0, 0), DedupKey: "check:presence:phone",
		Outcome: OutcomeFired, Reason: "transition",
		CheckObservations: `[{"observed_at":"2026-09-01T13:41:15Z","status":"up","consecutive":3}]`,
	})
	if err != nil {
		t.Fatalf("create event with ledger: %v", err)
	}
	got, err := s.GetEvent(ctx, ev.ID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if got.CheckObservations != `[{"observed_at":"2026-09-01T13:41:15Z","status":"up","consecutive":3}]` {
		t.Fatalf("embedded ledger not persisted: got %q", got.CheckObservations)
	}
}

// TestMigration0035UpgradesFrom34 pins a database at user_version 34 (no
// workspace.offline_providers column), then opens it with the current binary:
// the migration must add the column, default it off, and leave the store able
// to flip and read it through the CRUD.
func TestMigration0035UpgradesFrom34(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, path, 34)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var colCount int
	if err := raw.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('workspace') WHERE name = 'offline_providers'`).
		Scan(&colCount); err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("offline_providers column exists at user_version 34, want it introduced by 0035")
	}
	// Seed a workspace through the pre-migration schema so the upgrade has a
	// row to prove the new column defaults correctly on existing data.
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO workspace (id, name, tofu_dir, auto_apply, serial) VALUES (1, 'ws', '/t', 0, 0)`); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at 0034 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 35 {
		t.Fatalf("schema version = %d after open, want >= 35 (0035 applied)", version)
	}

	// The pre-existing row defaults to offline off.
	ws, err := s.WorkspaceByName(ctx, "ws")
	if err != nil {
		t.Fatalf("read seeded workspace: %v", err)
	}
	if ws.OfflineProviders {
		t.Fatalf("seeded workspace offline_providers = true, want the default false")
	}

	// The store can flip and read it.
	on, err := s.SetWorkspaceOfflineProviders(ctx, ws.ID, true)
	if err != nil {
		t.Fatalf("set offline on: %v", err)
	}
	if !on.OfflineProviders {
		t.Fatalf("workspace offline_providers not true after SetWorkspaceOfflineProviders(true)")
	}
	off, err := s.SetWorkspaceOfflineProviders(ctx, ws.ID, false)
	if err != nil {
		t.Fatalf("set offline off: %v", err)
	}
	if off.OfflineProviders {
		t.Fatalf("workspace offline_providers not false after SetWorkspaceOfflineProviders(false)")
	}
}

// TestMigration0036UpgradesFrom35 pins a database at user_version 35 (no
// artifact.plan_serial column), seeds an artifact row, then opens it with the
// current binary: the migration must add the column, default it to 0 for the
// pre-existing row, and leave the store able to record and read a plan serial.
func TestMigration0036UpgradesFrom35(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, path, 35)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var colCount int
	if err := raw.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('artifact') WHERE name = 'plan_serial'`).
		Scan(&colCount); err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if colCount != 0 {
		t.Fatalf("plan_serial column exists at user_version 35, want it introduced by 0036")
	}
	// Seed a job + an artifact through the pre-migration schema so the upgrade
	// has a row to prove the new column defaults correctly on existing data.
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO job (id, runbook, kind, "trigger", state, args_json, queued_at, owner_pid, partial)
		VALUES (1, 'workspace:ws', 'tofu', 'manual', 'succeeded', '{}', 1, 0, 0);
		INSERT INTO artifact (id, job_id, kind, path, sha256, size)
		VALUES (1, 1, 'tofu-plan', '/p/plan.tfplan.age', 'abc', 5)`); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at 0035 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 36 {
		t.Fatalf("schema version = %d after open, want >= 36 (0036 applied)", version)
	}

	// The pre-existing row defaults to serial 0 (unknown).
	pre, err := s.GetArtifact(ctx, 1)
	if err != nil {
		t.Fatalf("read seeded artifact: %v", err)
	}
	if pre.PlanSerial != 0 {
		t.Fatalf("seeded artifact plan_serial = %d, want the default 0", pre.PlanSerial)
	}

	// The store can record and read a plan serial.
	created, err := s.CreateArtifact(ctx, Artifact{
		JobID: 1, Kind: "tofu-plan", Path: "/p/p2.tfplan.age", SHA256: "def", Size: 7, PlanSerial: 42,
	})
	if err != nil {
		t.Fatalf("create artifact with plan_serial: %v", err)
	}
	got, err := s.GetArtifact(ctx, created.ID)
	if err != nil {
		t.Fatalf("get created artifact: %v", err)
	}
	if got.PlanSerial != 42 {
		t.Fatalf("artifact plan_serial = %d, want 42", got.PlanSerial)
	}
}

// TestMigration0037UpgradesFrom036 pins a database at user_version 36 (no
// check_state.netgroup), then opens it with the current binary: the column
// arrives in place and the store round-trips a stored classification.
func TestMigration0037UpgradesFrom036(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, path, 36)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var colCount int
	if err := raw.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('check_state') WHERE name = 'netgroup'`).Scan(&colCount); err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if colCount != 0 {
		t.Fatal("check_state.netgroup exists at user_version 36; the pin is not honest")
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at 0036 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 37 {
		t.Fatalf("schema version = %d after open, want >= 37 (0037 applied)", version)
	}

	c, err := s.CreateCheck(ctx, NewCheck{
		Name: "at-home", Kind: CheckTCP, IntervalS: 300,
		Targets: []CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create check on upgraded db: %v", err)
	}
	now := time.Now()
	if err := s.SetCheckState(ctx, CheckState{
		CheckID: c.ID, TargetName: "laptop", Status: CheckUp,
		Consecutive: 1, Emitted: CheckUp, FirstSeen: now, LastChange: now,
		Netgroup: "home",
	}); err != nil {
		t.Fatalf("set state on upgraded db: %v", err)
	}
	st, err := s.GetCheckState(ctx, c.ID, "laptop")
	if err != nil {
		t.Fatalf("get state on upgraded db: %v", err)
	}
	if st.Netgroup != "home" {
		t.Fatalf("netgroup = %q, want home", st.Netgroup)
	}
}

// TestMigration0038UpgradesFrom037 pins a database at user_version 37 (no
// check_state.last_duration_ms), then opens it with the current binary: the
// column arrives in place and the store round-trips a recorded duration.
func TestMigration0038UpgradesFrom037(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, path, 37)

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var colCount int
	if err := raw.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('check_state') WHERE name = 'last_duration_ms'`).Scan(&colCount); err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if colCount != 0 {
		t.Fatal("check_state.last_duration_ms exists at user_version 37; the pin is not honest")
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at 0037 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 38 {
		t.Fatalf("schema version = %d after open, want >= 38 (0038 applied)", version)
	}

	c, err := s.CreateCheck(ctx, NewCheck{
		Name: "at-home", Kind: CheckTCP, IntervalS: 300,
		Targets: []CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create check on upgraded db: %v", err)
	}
	now := time.Now()
	if err := s.SetCheckState(ctx, CheckState{
		CheckID: c.ID, TargetName: "laptop", Status: CheckUp,
		Consecutive: 1, Emitted: CheckUp, FirstSeen: now, LastChange: now,
		LastDurationMS: 57,
	}); err != nil {
		t.Fatalf("set state on upgraded db: %v", err)
	}
	st, err := s.GetCheckState(ctx, c.ID, "laptop")
	if err != nil {
		t.Fatalf("get state on upgraded db: %v", err)
	}
	if st.LastDurationMS != 57 {
		t.Fatalf("last_duration_ms = %d, want 57", st.LastDurationMS)
	}
}
