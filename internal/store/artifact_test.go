package store_test

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestListPlanArtifacts: the join returns only the plan artifacts of jobs
// whose runbook matches, newest artifact first and bounded by the limit —
// other artifact kinds and other runbooks stay out, and an absent runbook
// yields zero results rather than an error.
func TestListPlanArtifacts(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	mk := func(runbook, kind string) int64 {
		t.Helper()
		job, err := s.CreateJob(ctx, store.NewJob{Runbook: runbook, Kind: "tofu", Trigger: "manual"})
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		art, err := s.CreateArtifact(ctx, store.Artifact{JobID: job.ID, Kind: kind, Path: "/p", SHA256: "x", Size: 1})
		if err != nil {
			t.Fatalf("create artifact: %v", err)
		}
		return art.ID
	}

	prod := []int64{
		mk("workspace:prod", "tofu-plan"),
		mk("workspace:prod", "tofu-plan"),
		mk("workspace:prod", "tofu-plan"),
	}
	mk("workspace:prod", "tofu-log")
	mk("workspace:staging", "tofu-plan")

	limited, err := s.ListPlanArtifacts(ctx, "workspace:prod", "tofu-plan", 2)
	if err != nil {
		t.Fatalf("ListPlanArtifacts (limited): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("len = %d, want 2 (limit)", len(limited))
	}
	want := []int64{prod[2], prod[1]}
	for i, w := range want {
		if limited[i].ID != w {
			t.Errorf("limited[%d].ID = %d, want %d", i, limited[i].ID, w)
		}
	}

	all, err := s.ListPlanArtifacts(ctx, "workspace:prod", "tofu-plan", 20)
	if err != nil {
		t.Fatalf("ListPlanArtifacts (unlimited): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3 plans for workspace:prod", len(all))
	}
	for i, w := range []int64{prod[2], prod[1], prod[0]} {
		if all[i].ID != w {
			t.Errorf("all[%d].ID = %d, want %d (newest first)", i, all[i].ID, w)
		}
	}

	none, err := s.ListPlanArtifacts(ctx, "workspace:absent", "tofu-plan", 20)
	if err != nil {
		t.Fatalf("ListPlanArtifacts (absent runbook): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("absent runbook returned %d artifacts, want none", len(none))
	}
}
