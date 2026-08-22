package sched

import (
	"context"
	"slices"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// TestTofuPlanScheduleFiresADriftCheck: the tofu-plan kind enqueues an
// ordinary tofu job against the workspace it names, keyed so it cannot run
// beside another operation on the same state.
func TestTofuPlanScheduleFiresADriftCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st := newTestStore(t)
	if _, err := st.CreateWorkspace(ctx, store.Workspace{
		Name: "prod", TofuDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	sc, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name:     "drift-prod",
		CronExpr: "@every 1h",
		Kind:     store.ScheduleTofuPlan,
		Args:     store.ScheduleArgs{Target: "prod"},
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	s := New(st, &fakeEnqueuer{store: st}, testLogger())
	specs, err := s.buildSpecs(ctx, sc)
	if err != nil {
		t.Fatalf("build specs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("built %d specs, want 1", len(specs))
	}

	spec := specs[0]
	if spec.Kind != executor.KindTofu {
		t.Fatalf("spec kind = %q, want %q", spec.Kind, executor.KindTofu)
	}
	if spec.Runbook != "workspace:prod" {
		t.Fatalf("runbook = %q, want the daemon to read the workspace back out of it", spec.Runbook)
	}
	if spec.ConcurrencyKey != "tofu:prod" {
		t.Fatalf("concurrency key = %q, want a drift check to serialize with applies", spec.ConcurrencyKey)
	}
	if spec.Trigger != Trigger {
		t.Fatalf("trigger = %q, want %q", spec.Trigger, Trigger)
	}
	if !slices.Contains(spec.Argv, "plan") {
		t.Fatalf("argv = %v, want a plan", spec.Argv)
	}
}

func TestTofuPlanScheduleNeedsAKnownWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)
	s := New(st, &fakeEnqueuer{store: st}, testLogger())

	noWorkspace, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "drift-none", CronExpr: "@every 1h", Kind: store.ScheduleTofuPlan,
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if _, err := s.buildSpecs(ctx, noWorkspace); err == nil {
		t.Fatal("a tofu-plan schedule with no workspace built a spec, want an error")
	}

	unknown, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "drift-ghost", CronExpr: "@every 1h", Kind: store.ScheduleTofuPlan,
		Args: store.ScheduleArgs{Target: "ghost"},
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if _, err := s.buildSpecs(ctx, unknown); err == nil {
		t.Fatal("a tofu-plan schedule naming no known workspace built a spec, want an error")
	}
}
