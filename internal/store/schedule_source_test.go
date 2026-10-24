package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func openScheduleSourceStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestScheduleSourceRoundTrips: a play schedule's source persists beside its
// other args and reads back from both the single and list reads. The source
// rides in args_json, so no migration is involved.
func TestScheduleSourceRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openScheduleSourceStore(t)

	created, err := s.CreateSchedule(ctx, NewSchedule{
		Name: "converge", CronExpr: "@every 1h", Kind: "play",
		Args: ScheduleArgs{
			Argv: []string{"plays/site.yml", "--check", "--diff"}, Target: "tf:edge", Source: "infra",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Args.Source != "infra" {
		t.Fatalf("created source = %q, want infra", created.Args.Source)
	}

	got, err := s.GetSchedule(ctx, "converge")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Args.Source != "infra" || got.Args.Target != "tf:edge" || len(got.Args.Argv) != 3 {
		t.Fatalf("got args = %+v", got.Args)
	}

	all, err := s.ListSchedules(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].Args.Source != "infra" {
		t.Fatalf("list = %+v, want the one schedule carrying source infra", all)
	}
}

// TestScheduleWithoutSourceKeepsItsStoredShape: a schedule with no source
// writes no "source" key, so every existing row and every new sourceless row
// stores exactly what it did before the field existed.
func TestScheduleWithoutSourceKeepsItsStoredShape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openScheduleSourceStore(t)

	if _, err := s.CreateSchedule(ctx, NewSchedule{
		Name: "converge", CronExpr: "@every 1h", Kind: "play",
		Args: ScheduleArgs{Argv: []string{"/srv/site.yml"}, Target: "tf:edge"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx,
		`SELECT args_json FROM schedule WHERE name = 'converge'`).Scan(&raw); err != nil {
		t.Fatalf("read args_json: %v", err)
	}
	if strings.Contains(raw, "source") {
		t.Fatalf("args_json = %s, want no source key for a sourceless schedule", raw)
	}
}

// TestLegacyScheduleRowDecodesWithoutSource: a row written before the field
// existed, with no migration applied, reads back with an empty source.
func TestLegacyScheduleRowDecodesWithoutSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openScheduleSourceStore(t)

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO schedule (name, cron_expr, kind, args_json, concurrency_key, jitter_s, enabled)
		VALUES ('legacy', '@every 1h', 'play',
		        '{"argv":["/srv/site.yml"],"target":"tf:edge","tolerate_unreachable":true}', '', 0, 1)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	got, err := s.GetSchedule(ctx, "legacy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Args.Source != "" || !got.Args.TolerateUnreachable || got.Args.Target != "tf:edge" {
		t.Fatalf("legacy args = %+v, want the old fields intact and no source", got.Args)
	}
}
