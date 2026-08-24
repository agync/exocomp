package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0025UpgradesFrom24 pins a database at user_version 24 (no
// event.job_id index), seeds an event the pre-migration schema already
// supports, then opens it with the current binary: the migration must apply
// cleanly and EventByJobID must still find the row it wrote before the index
// existed.
func TestMigration0025UpgradesFrom24(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, dbPath, 24)

	raw, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var indexCount int
	err = raw.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'event_job_id'`).
		Scan(&indexCount)
	if err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if indexCount != 0 {
		t.Fatalf("event_job_id index exists at user_version 24, want it introduced by 0025")
	}

	// Seed a job and an event naming it, through the schema as it stood before
	// this migration, so the upgrade has a pre-existing row to prove it still
	// finds.
	res, err := raw.ExecContext(ctx, `
		INSERT INTO job (runbook, kind, "trigger", state, args_json, queued_at,
		                 concurrency_key, dedup_key, idempotency_key, owner_pid)
		VALUES ('backup', 'backup', 'schedule', 'queued', '{}', 0, '', NULL, NULL, 0)`)
	if err != nil {
		t.Fatalf("seed pre-migration job: %v", err)
	}
	jobID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("job id: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO event (source, received_at, dedup_key, normalized_json, raw_json,
		                   matched_rule, outcome, reason, job_id, trace_json)
		VALUES ('backup', 1000, '', '{}', '{}', 'nightly', 'fired', '', ?, '[]')`, jobID); err != nil {
		t.Fatalf("seed pre-migration event: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open at v24 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 25 {
		t.Fatalf("schema version = %d after open, want >= 25", version)
	}

	err = s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'event_job_id'`).
		Scan(&indexCount)
	if err != nil {
		t.Fatalf("probe post-migration schema: %v", err)
	}
	if indexCount != 1 {
		t.Fatalf("event_job_id index count = %d after migration, want 1", indexCount)
	}

	got, err := s.EventByJobID(ctx, jobID)
	if err != nil {
		t.Fatalf("EventByJobID on migrated schema: %v", err)
	}
	if got.MatchedRule != "nightly" || got.Outcome != OutcomeFired {
		t.Fatalf("event by job id = %+v, want the pre-migration row", got)
	}
}

// TestEventByJobIDUsesIndex proves EventByJobID's query plan reads through the
// new index rather than scanning the whole event table (finding: a job_id
// lookup here was an O(n) scan with no index to bound it).
func TestEventByJobIDUsesIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	job, err := s.CreateJob(ctx, NewJob{
		Runbook: "backup", Kind: "backup", Trigger: "schedule",
		Args: JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	jobID := job.ID
	if _, err := s.CreateEvent(ctx, Event{
		Source: "backup", MatchedRule: "nightly", Outcome: OutcomeFired, JobID: &jobID,
	}); err != nil {
		t.Fatalf("create event: %v", err)
	}

	row := s.db.QueryRowContext(ctx, `EXPLAIN QUERY PLAN SELECT `+eventColumns+
		` FROM event WHERE job_id = ? ORDER BY received_at DESC, id DESC LIMIT 1`, jobID)
	var (
		selectID, order, notUsed int
		detail                   string
	)
	if err := row.Scan(&selectID, &order, &notUsed, &detail); err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	if !strings.Contains(detail, "event_job_id") {
		t.Fatalf("query plan = %q, want it to use the event_job_id index", detail)
	}
	if strings.Contains(strings.ToUpper(detail), "SCAN") {
		t.Fatalf("query plan = %q, EventByJobID must SEARCH the index, not SCAN the table", detail)
	}
}
