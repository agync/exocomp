package main

import (
	"context"
	"os/exec"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestTwoProcessesSerializeAConcurrencyKey drives two real CLI processes at
// one data dir. Nothing in either process's memory knows about the other, so
// only the database can keep two jobs that share a key — two restic backups of
// one repository, say — from running at once.
func TestTwoProcessesSerializeAConcurrencyKey(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	bin := buildExocomp(t)

	runCLI(t, bin, "init")

	const key = "backup:nas"
	done := make(chan error, 2)
	for range 2 {
		go func() {
			done <- exec.Command(bin, "devrun", "--concurrency-key", key,
				"--", "/bin/sleep", "2").Run()
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("devrun: %v", err)
		}
	}

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	jobs, err := st.ListJobs(context.Background(),
		store.JobFilter{ConcurrencyKey: key})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("%d job(s) carry the key, want 2", len(jobs))
	}
	first, second := jobs[0], jobs[1]
	if first.StartedAt == nil || first.FinishedAt == nil ||
		second.StartedAt == nil || second.FinishedAt == nil {
		t.Fatalf("a job is missing its timestamps: %+v %+v", first, second)
	}
	if second.StartedAt.Before(*first.FinishedAt) && first.StartedAt.Before(*second.FinishedAt) {
		t.Errorf("two processes ran one concurrency key at once: job %d [%s, %s], job %d [%s, %s]",
			first.ID, first.StartedAt, first.FinishedAt,
			second.ID, second.StartedAt, second.FinishedAt)
	}
}
