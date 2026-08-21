package sched

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

func TestValidateCron(t *testing.T) {
	t.Parallel()

	valid := []string{"@every 1s", "@daily", "0 0 3 * * *", "*/5 * * * * *"}
	for _, expr := range valid {
		if err := ValidateCron(expr); err != nil {
			t.Errorf("ValidateCron(%q) = %v, want nil", expr, err)
		}
	}

	invalid := []string{"", "not a cron at all", "0 3 * * *", "@every", "@sometimes", "99 * * * * *"}
	for _, expr := range invalid {
		if err := ValidateCron(expr); err == nil {
			t.Errorf("ValidateCron(%q) = nil, want an error", expr)
		}
	}
}

// createSchedule writes a row straight to the store, which is how a bad cron
// expression gets past the CLI's validation — an older binary wrote it, or an
// operator edited the database.
func createSchedule(t *testing.T, st *store.Store, name, expr string) store.Schedule {
	t.Helper()
	sc, err := st.CreateSchedule(t.Context(), store.NewSchedule{
		Name:     name,
		CronExpr: expr,
		Kind:     store.ScheduleShell,
		Args:     store.ScheduleArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create schedule %s: %v", name, err)
	}
	return sc
}

// TestStartSkipsAnUnregisterableSchedule is the daemon's startup contract: one
// row nothing can parse must cost that row its registration, not the queue and
// the state backend that start alongside the scheduler.
func TestStartSkipsAnUnregisterableSchedule(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	createSchedule(t, st, "junk", "not a cron at all")
	createSchedule(t, st, "good", "@every 1h")

	var logged bytes.Buffer
	s := New(st, &fakeEnqueuer{store: st}, slog.New(slog.NewTextHandler(&logged, nil)))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start refused to run because one row is bad: %v", err)
	}
	t.Cleanup(s.Stop)

	if got := len(s.entries); got != 1 {
		t.Fatalf("registered entries = %d, want 1 (the parseable schedule only)", got)
	}
	if !strings.Contains(logged.String(), "junk") {
		t.Errorf("the skipped schedule was not logged:\n%s", logged.String())
	}
}

// TestReloadKeepsTheOldSetWhenItCannotBuildANewOne covers the other failure:
// a reload that cannot read the database used to unregister everything on its
// way to returning the error, leaving a live daemon with no schedules at all.
func TestReloadKeepsTheOldSetWhenItCannotBuildANewOne(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	createSchedule(t, st, "a", "@every 1h")
	createSchedule(t, st, "b", "@every 2h")

	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	if got := len(s.entries); got != 2 {
		t.Fatalf("entries after start = %d, want 2", got)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := s.Reload(context.Background()); err == nil {
		t.Fatal("reload against a closed store returned nil")
	}
	if got := len(s.entries); got != 2 {
		t.Errorf("entries after a failed reload = %d, want the 2 that were running", got)
	}
	if len(s.cron.Entries()) != 2 {
		t.Errorf("the cron holds %d entries after a failed reload, want 2", len(s.cron.Entries()))
	}
}
