package store_test

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestJobPartialRoundTrip proves the additive partial flag persists and defaults
// off, and that TolerateUnreachable survives the args round-trip.
func TestJobPartialRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := open(t)

	job, err := s.CreateJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: "ansible", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"ansible-playbook", "site.yml"}, TolerateUnreachable: true},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job.Partial {
		t.Errorf("a fresh job is partial; the flag must default off")
	}
	if !job.Args.TolerateUnreachable {
		t.Errorf("TolerateUnreachable did not survive the args round-trip")
	}

	if err := s.SetJobPartial(ctx, job.ID, true); err != nil {
		t.Fatalf("set job partial: %v", err)
	}
	got, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !got.Partial {
		t.Errorf("Partial = false after SetJobPartial(true)")
	}
}
