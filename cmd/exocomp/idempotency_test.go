package main

import (
	"bytes"
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestIdempotencyKeyDedupsRetriedJob drives one mutating command (devrun) three
// times over one data dir: the same --idempotency-key twice must dedup to the
// SAME job (a retry never double-enqueues), while a different key is a new job.
func TestIdempotencyKeyDedupsRetriedJob(t *testing.T) {
	useDataDir(t, t.TempDir())

	run := func(key string) {
		t.Helper()
		var out bytes.Buffer
		if err := runDevRun(context.Background(), &out, devRunSpec{
			argv:           []string{"/bin/true"},
			idempotencyKey: key,
		}); err != nil {
			t.Fatalf("devrun %q: %v (%s)", key, err, out.String())
		}
	}

	run("intent-A")
	run("intent-A") // retry of the same intent
	run("intent-B")

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	jobs, err := st.ListJobs(ctx, store.JobFilter{Runbook: "devrun"})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want 2 devrun jobs (intent-A deduped, intent-B new), got %d", len(jobs))
	}

	a, err := st.JobByIdempotencyKey(ctx, "intent-A")
	if err != nil {
		t.Fatalf("job by key intent-A: %v", err)
	}
	b, err := st.JobByIdempotencyKey(ctx, "intent-B")
	if err != nil {
		t.Fatalf("job by key intent-B: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("different keys mapped to one job %d", a.ID)
	}
}

// TestNoIdempotencyKeyEnqueuesEveryTime pins the unchanged default: with no key
// each run is its own job, exactly as before the flag existed.
func TestNoIdempotencyKeyEnqueuesEveryTime(t *testing.T) {
	useDataDir(t, t.TempDir())

	for range 2 {
		var out bytes.Buffer
		if err := runDevRun(context.Background(), &out, devRunSpec{
			argv: []string{"/bin/true"},
		}); err != nil {
			t.Fatalf("devrun: %v (%s)", err, out.String())
		}
	}

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Runbook: "devrun"})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want 2 independent devrun jobs, got %d", len(jobs))
	}
}
