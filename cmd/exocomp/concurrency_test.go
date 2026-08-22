package main

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestTwoProcessesSerializeAConcurrencyKey drives two real CLI processes at
// one data dir. Nothing in either process's memory knows about the other, so
// only the database can keep two jobs that share a key — two restic backups of
// one repository, say — from running at once.
func TestTwoProcessesSerializeAConcurrencyKey(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)

	runCLI(t, bin, "init")
	// One job first, so both processes below meet a database that exists and
	// is already migrated: what is under test is the claim, not the open.
	runCLI(t, bin, "devrun", "--", "/bin/true")

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
	// Serialization is SQL-enforced (a second claim cannot succeed while the key
	// is held), so genuinely-concurrent same-key jobs would overlap for a whole
	// job duration (~seconds). The recorded start/finish wall-clocks can still
	// invert by well under a millisecond between two correctly-serialized jobs
	// under load, so measure the overlap and only fail on a real one, not on
	// timestamp jitter.
	const overlapTolerance = 250 * time.Millisecond
	overlapStart := *first.StartedAt
	if second.StartedAt.After(overlapStart) {
		overlapStart = *second.StartedAt
	}
	overlapEnd := *first.FinishedAt
	if second.FinishedAt.Before(overlapEnd) {
		overlapEnd = *second.FinishedAt
	}
	if overlap := overlapEnd.Sub(overlapStart); overlap > overlapTolerance {
		t.Errorf("two processes ran one concurrency key at once (overlap %s): job %d [%s, %s], job %d [%s, %s]",
			overlap, first.ID, first.StartedAt, first.FinishedAt,
			second.ID, second.StartedAt, second.FinishedAt)
	}
}
