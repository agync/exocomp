package queue_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// degradedDriver reports a tolerated degradation: a non-zero exit whose only
// non-success the driver already judged tolerable, as the ansible driver does
// for an unreachable-only run.
type degradedDriver struct{}

func (degradedDriver) Run(_ context.Context, _ executor.Spec, _ executor.EventSink) (executor.Result, error) {
	return executor.Result{ExitCode: 4, Degraded: true, UnreachableHosts: []string{"laptop"}}, nil
}

// TestDegradedRunSettlesSucceededPartial proves a Degraded driver result settles
// the job succeeded (so it does not page) and marks the row partial (so the
// degraded success is told from a clean one), while the true exit code stays.
func TestDegradedRunSettlesSucceededPartial(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	q := queue.New(st, degradedDriver{}, queue.Options{
		Workers:      1,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Argv:                []string{"ansible-playbook", "site.yml"},
		TolerateUnreachable: true,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := q.Drain(dctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	final, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if final.State != store.StateSucceeded {
		t.Errorf("state = %s, want succeeded (a tolerated degradation must not fail)", final.State)
	}
	if !final.Partial {
		t.Errorf("Partial = false, want true (the degraded success must be marked)")
	}
	if final.ExitCode == nil || *final.ExitCode != 4 {
		t.Errorf("exit code = %v, want the real 4 preserved", final.ExitCode)
	}
}
