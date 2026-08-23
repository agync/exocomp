package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestMigration0021UpgradesFrom20 pins a database at user_version 20, seeds a
// schedule under the pre-0021 schema (no last_run column), then opens it with
// this binary. An operator upgrading in place must get the new column defaulted
// to NULL on the pre-existing row — so its first (re)load treats it as
// never-fired and does not back-fire — and a usable schedule table afterward.
func TestMigration0021UpgradesFrom20(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, dbPath, 20)

	raw, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	// The pre-0021 schedule row has no last_run column at all.
	var cols int
	if err := raw.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('schedule') WHERE name = 'last_run'`).Scan(&cols); err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if cols != 0 {
		t.Fatalf("last_run column exists at user_version 20, want it introduced by 0021")
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO schedule (name, cron_expr, kind, args_json, concurrency_key, jitter_s, enabled)
		VALUES ('legacy', '@every 1h', 'shell', '{"argv":["/bin/true"]}', '', 0, 1)`); err != nil {
		t.Fatalf("insert pre-migration schedule: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open at v20 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 21 {
		t.Fatalf("schema version = %d after open, want >= 21", version)
	}

	legacy, err := s.GetSchedule(ctx, "legacy")
	if err != nil {
		t.Fatalf("get migrated schedule: %v", err)
	}
	if legacy.LastRun != nil {
		t.Errorf("migrated schedule last_run = %v, want nil (never fired)", legacy.LastRun)
	}

	// The table is usable after the upgrade: create, stamp last_run, read back.
	created, err := s.CreateSchedule(ctx, NewSchedule{
		Name: "fresh", CronExpr: "@every 1h", Kind: ScheduleShell,
		Args: ScheduleArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create schedule on migrated table: %v", err)
	}
	if created.LastRun != nil {
		t.Errorf("new schedule last_run = %v, want nil", created.LastRun)
	}

	want := time.Now().Add(-90 * time.Minute).Truncate(time.Millisecond)
	if err := s.SetScheduleLastRun(ctx, created.ID, want); err != nil {
		t.Fatalf("set last_run: %v", err)
	}
	got, err := s.GetScheduleByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.LastRun == nil || !got.LastRun.Equal(want.UTC()) {
		t.Fatalf("last_run = %v, want %v", got.LastRun, want.UTC())
	}
}

// TestRecordScheduleFireAdvancesLastRun proves the live fire path stamps
// last_run to now even when the fire enqueued nothing to point last_job_id at.
func TestRecordScheduleFireAdvancesLastRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sc, err := s.CreateSchedule(ctx, NewSchedule{
		Name: "sc", CronExpr: "@every 1h", Kind: ScheduleShell,
		Args: ScheduleArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if sc.LastRun != nil {
		t.Fatalf("fresh schedule last_run = %v, want nil", sc.LastRun)
	}

	before := time.Now().Add(-time.Second)
	if err := s.RecordScheduleFire(ctx, sc.ID, nil, nil); err != nil {
		t.Fatalf("record fire: %v", err)
	}
	got, err := s.GetScheduleByID(ctx, sc.ID)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if got.LastRun == nil {
		t.Fatal("last_run still nil after a fire")
	}
	if got.LastRun.Before(before) {
		t.Fatalf("last_run = %v, want >= %v (advanced to now)", got.LastRun, before)
	}
}
