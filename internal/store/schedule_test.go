package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

func TestScheduleCRUD(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	created, err := s.CreateSchedule(ctx, store.NewSchedule{
		Name:     "nightly",
		CronExpr: "@every 24h",
		Kind:     store.ScheduleShell,
		Args:     store.ScheduleArgs{Argv: []string{"/bin/true"}},
		JitterS:  30,
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if created.ID == 0 || !created.Enabled {
		t.Fatalf("created = %+v", created)
	}
	if len(created.Args.Argv) != 1 || created.Args.Argv[0] != "/bin/true" {
		t.Fatalf("args = %+v", created.Args)
	}

	if _, err := s.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly", CronExpr: "@every 1h", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"x"}},
	}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate name err = %v, want ErrDuplicate", err)
	}

	got, err := s.GetSchedule(ctx, "nightly")
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if got.CronExpr != "@every 24h" || got.JitterS != 30 {
		t.Fatalf("got = %+v", got)
	}

	if _, err := s.GetSchedule(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get missing err = %v, want ErrNotFound", err)
	}

	backupSched, err := s.CreateSchedule(ctx, store.NewSchedule{
		Name: "backup-all", CronExpr: "@every 1h", Kind: store.ScheduleBackup,
		Args: store.ScheduleArgs{Target: ""},
	})
	if err != nil {
		t.Fatalf("create backup schedule: %v", err)
	}

	all, err := s.ListSchedules(ctx, false)
	if err != nil {
		t.Fatalf("list schedules: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list schedules = %d, want 2", len(all))
	}

	job, err := s.CreateJob(ctx, store.NewJob{Kind: "shell", Args: store.JobArgs{Argv: []string{"/bin/true"}}})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	next := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Millisecond)
	jobID := job.ID
	if err := s.RecordScheduleFire(ctx, created.ID, &jobID, &next); err != nil {
		t.Fatalf("record schedule fire: %v", err)
	}

	fired, err := s.GetSchedule(ctx, "nightly")
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if fired.LastJobID == nil || *fired.LastJobID != job.ID {
		t.Fatalf("last job id = %v, want %d", fired.LastJobID, job.ID)
	}
	if fired.NextAt == nil || !fired.NextAt.Equal(next) {
		t.Fatalf("next at = %v, want %v", fired.NextAt, next)
	}

	if err := s.DeleteSchedule(ctx, "backup-all"); err != nil {
		t.Fatalf("delete schedule: %v", err)
	}
	if _, err := s.GetSchedule(ctx, "backup-all"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get deleted schedule err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSchedule(ctx, "backup-all"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete already-deleted schedule err = %v, want ErrNotFound", err)
	}

	_ = backupSched
}

func TestListSchedulesEnabledOnly(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	if _, err := s.CreateSchedule(ctx, store.NewSchedule{
		Name: "a", CronExpr: "@every 1h", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"x"}},
	}); err != nil {
		t.Fatalf("create schedule a: %v", err)
	}
	if _, err := s.CreateSchedule(ctx, store.NewSchedule{
		Name: "b", CronExpr: "@every 1h", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"x"}},
	}); err != nil {
		t.Fatalf("create schedule b: %v", err)
	}

	enabled, err := s.ListSchedules(ctx, true)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(enabled) != 2 {
		t.Fatalf("enabled schedules = %d, want 2", len(enabled))
	}

	if err := s.SetScheduleEnabled(ctx, "a", false); err != nil {
		t.Fatalf("disable a: %v", err)
	}
	afterDisable, err := s.ListSchedules(ctx, true)
	if err != nil {
		t.Fatalf("list enabled after disable: %v", err)
	}
	if len(afterDisable) != 1 || afterDisable[0].Name != "b" {
		t.Fatalf("enabled schedules after disabling a = %+v", afterDisable)
	}

	all, err := s.ListSchedules(ctx, false)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all schedules = %d, want 2 (disabled rows stay)", len(all))
	}

	if err := s.SetScheduleEnabled(ctx, "missing", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("enable missing err = %v, want ErrNotFound", err)
	}
}
