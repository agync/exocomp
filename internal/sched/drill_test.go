package sched

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

func TestBuildSpecsDrillSingleTarget(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	sc := store.Schedule{Name: "one", Kind: store.ScheduleDrill, Args: store.ScheduleArgs{Target: "nas"}}
	specs, err := s.buildSpecs(t.Context(), sc)
	if err != nil {
		t.Fatalf("buildSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	got := specs[0]
	if got.Kind != backup.DrillJobKind || got.ConcurrencyKey != backup.DrillConcurrencyKey("nas") {
		t.Fatalf("spec = %+v", got)
	}
	if name, err := backup.DrillTargetOf(got.Argv); err != nil || name != "nas" {
		t.Fatalf("argv target = %q, %v, want nas", name, err)
	}
}

func TestBuildSpecsDrillAllTargetsFansOutSkippingDisabled(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	for _, target := range []struct {
		name    string
		enabled bool
	}{
		{"nas", true},
		{"offsite", true},
		{"retired", false},
	} {
		if _, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
			Name: target.name, Kind: "restic_local", URI: t.TempDir(), Enabled: target.enabled,
		}); err != nil {
			t.Fatalf("create target %s: %v", target.name, err)
		}
	}

	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	sc := store.Schedule{Name: "all", Kind: store.ScheduleDrill}
	specs, err := s.buildSpecs(t.Context(), sc)
	if err != nil {
		t.Fatalf("buildSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2 (enabled targets only)", len(specs))
	}
	keys := map[string]bool{}
	for _, spec := range specs {
		keys[spec.ConcurrencyKey] = true
	}
	if !keys[backup.DrillConcurrencyKey("nas")] || !keys[backup.DrillConcurrencyKey("offsite")] {
		t.Fatalf("concurrency keys = %v", keys)
	}
	if keys[backup.DrillConcurrencyKey("retired")] {
		t.Fatal("disabled target got a spec")
	}
}

func TestBuildSpecsDrillUnknownKindStillRejected(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	sc := store.Schedule{Name: "bogus", Kind: "not-a-kind"}
	if _, err := s.buildSpecs(t.Context(), sc); err == nil {
		t.Fatal("unknown schedule kind: want error")
	}
}

// TestScheduledDrillFiresThroughQueue proves a kind-drill schedule fire
// reaches a real backup target end to end: the exact spec buildSpecs would
// hand a cron tick, enqueued into a real queue behind the same NewDriver a
// live daemon wires up (internal/sched/driver.go), settles the job it
// produced succeeded — the monthly dr-drill (disaster-recovery.md) as an
// unattended job instead of a manual `exocomp backup drill`.
func TestScheduledDrillFiresThroughQueue(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not installed")
	}

	dataDir := filepath.Join(t.TempDir(), "data")
	env, _, err := secrets.Init(dataDir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })

	st, err := store.Open(filepath.Join(dataDir, "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cache := t.TempDir()
	targetEnv := func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": cache}
	}

	target, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "nas", Kind: "restic_local", URI: filepath.Join(t.TempDir(), "repo"), Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	runner := backup.NewRunner(st, env, backup.Options{}, targetEnv)
	driver := NewDriver(executor.NewShell(), runner)
	q := queue.New(st, driver, queue.Options{Workers: 2, PollInterval: 10 * time.Millisecond})

	// Seed a real snapshot the same way `backup run` would, through the same
	// queue and driver a drill fire below reuses.
	if _, err := q.Enqueue(t.Context(), queue.Spec{
		Runbook: "backup", Kind: KindBackup, Trigger: "manual",
		Argv: backup.Argv(target.Name), ConcurrencyKey: backup.ConcurrencyKey(target.Name),
	}); err != nil {
		t.Fatalf("enqueue backup: %v", err)
	}
	if err := q.Drain(t.Context()); err != nil {
		t.Fatalf("drain backup: %v", err)
	}

	s := New(st, q, testLogger())
	sc := store.Schedule{Name: "monthly-drill", Kind: store.ScheduleDrill, Args: store.ScheduleArgs{Target: target.Name}}
	specs, err := s.buildSpecs(t.Context(), sc)
	if err != nil {
		t.Fatalf("buildSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}

	job, err := q.Enqueue(t.Context(), specs[0])
	if err != nil {
		t.Fatalf("enqueue drill: %v", err)
	}
	if err := q.Drain(t.Context()); err != nil {
		t.Fatalf("drain drill: %v", err)
	}

	got, err := st.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Kind != backup.DrillJobKind {
		t.Fatalf("drill job kind = %q, want %q", got.Kind, backup.DrillJobKind)
	}
	if got.State != store.StateSucceeded {
		events, _ := st.ListJobEvents(t.Context(), job.ID)
		t.Fatalf("drill job state = %s, want succeeded (events: %+v)", got.State, events)
	}
}
