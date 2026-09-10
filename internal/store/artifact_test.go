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

// TestReRecorded pins the CLI apply's consumed-plan dedupe (SPEC §2.2): an
// apply re-records a sealed plan under the applying job's id, and a SECOND
// apply of the same plan must not pile up another row. ReRecorded reads "is
// this sha already linked to a job other than the one that sealed it?" — false
// for the seal alone, true once any apply re-records it, and still true after
// an unrelated plan re-records. An operator's own plan file (sealJob 0) reads
// the same way once an apply re-records it.
func TestReRecorded(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	mk := func(runbook, kind, sha string) (int64, int64) {
		t.Helper()
		job, err := s.CreateJob(ctx, store.NewJob{Runbook: runbook, Kind: "tofu", Trigger: "manual"})
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		art, err := s.CreateArtifact(ctx, store.Artifact{JobID: job.ID, Kind: kind, Path: "/p", SHA256: sha, Size: 1})
		if err != nil {
			t.Fatalf("create artifact: %v", err)
		}
		return art.ID, job.ID
	}

	// The seal is recorded by the plan job; nothing else shares its sha yet.
	_, planJob := mk("workspace:web", "tofu-plan", "sha-a")
	got, err := s.ReRecorded(ctx, "sha-a", planJob)
	if err != nil || got {
		t.Fatalf("ReRecorded(seal alone) = %v,%v want false", got, err)
	}

	// An apply re-records the same sealed plan under the applying job.
	_, applyJob := mk("workspace:web", "tofu-plan", "sha-a")
	got, err = s.ReRecorded(ctx, "sha-a", planJob)
	if err != nil || !got {
		t.Fatalf("ReRecorded(once applied) = %v,%v want true", got, err)
	}
	_ = applyJob

	// An operator's own plan file was never sealed by a job (sealJob 0) and has
	// no artifact row; the first apply re-records it, the second must not.
	got, err = s.ReRecorded(ctx, "sha-b", 0)
	if err != nil || got {
		t.Fatalf("ReRecorded(own seal absent) = %v,%v want false", got, err)
	}
	_, ownApplyJob := mk("workspace:own", "tofu-plan", "sha-b")
	got, err = s.ReRecorded(ctx, "sha-b", 0)
	if err != nil || !got {
		t.Fatalf("ReRecorded(own re-recorded) = %v,%v want true", got, err)
	}
	_ = ownApplyJob
}
