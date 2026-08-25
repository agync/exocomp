package queue_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

func newQueue(t *testing.T, workers int) (*queue.Queue, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      workers,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return q, st
}

func shellSpec(argv ...string) queue.Spec {
	return queue.Spec{Runbook: "devrun", Kind: "shell", Trigger: "manual", Argv: argv}
}

func drain(t *testing.T, q *queue.Queue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := q.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func TestDrainRunsJobsToCompletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 2)

	ok, err := q.Enqueue(ctx, shellSpec("/bin/echo", "hello"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	bad, err := q.Enqueue(ctx, shellSpec("/bin/false"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	drain(t, q)

	succeeded, err := st.GetJob(ctx, ok.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if succeeded.State != store.StateSucceeded {
		t.Errorf("state = %s, want succeeded", succeeded.State)
	}
	if succeeded.ExitCode == nil || *succeeded.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", succeeded.ExitCode)
	}
	if succeeded.StartedAt == nil || succeeded.FinishedAt == nil {
		t.Errorf("job lacks timestamps: %+v", succeeded)
	}

	failed, err := st.GetJob(ctx, bad.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if failed.State != store.StateFailed {
		t.Errorf("state = %s, want failed", failed.State)
	}
	if failed.ExitCode == nil || *failed.ExitCode != 1 {
		t.Errorf("exit code = %v, want 1", failed.ExitCode)
	}

	events, err := st.ListJobEvents(ctx, ok.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var stdout []string
	for _, e := range events {
		if e.Level == store.LevelInfo {
			stdout = append(stdout, e.Msg)
		}
	}
	if len(stdout) != 1 || stdout[0] != "hello" {
		t.Errorf("stdout events = %q", stdout)
	}
	if last := events[len(events)-1]; !strings.Contains(last.Msg, "succeeded") {
		t.Errorf("final event = %q", last.Msg)
	}
}

// TestEnqueueCarriesRunnerName proves Spec.RunnerName reaches the stored job
// row: the seam a job-creation command's --runner flag depends on to reach
// resolveRunnerCaps at dispatch time (cmd/exocomp). Empty is the default,
// unaffected path most jobs take.
func TestEnqueueCarriesRunnerName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := shellSpec("/bin/true")
	spec.RunnerName = "capped-runner"
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Args.RunnerName != "capped-runner" {
		t.Errorf("stored RunnerName = %q, want %q", got.Args.RunnerName, "capped-runner")
	}
}

// TestEnqueueCarriesPrivileged proves Spec.Privileged reaches the stored job
// row, so whichever process claims the job runs it unsandboxed as the requester
// asked. Off by default is the normal-tiering path every other job takes.
func TestEnqueueCarriesPrivileged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := shellSpec("/bin/true")
	spec.Privileged = true
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !got.Args.Privileged {
		t.Errorf("stored Privileged = false, want true")
	}
}

// TestEnqueueCarriesSource proves Spec.Source reaches the stored job row, the
// seam a --source run's job event line and every job face read the source
// name back from. Empty is the default, unaffected path a raw-path or ad-hoc
// run takes.
func TestEnqueueCarriesSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := shellSpec("/bin/true")
	spec.Source = "my-fleet"
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Args.Source != "my-fleet" {
		t.Errorf("stored Source = %q, want %q", got.Args.Source, "my-fleet")
	}
}

// recordingDriver captures the last Spec it was handed, so a test can assert
// what the queue built from a job row reached the driver.
type recordingDriver struct{ last executor.Spec }

func (d *recordingDriver) Run(_ context.Context, spec executor.Spec, _ executor.EventSink) (executor.Result, error) {
	d.last = spec
	return executor.Result{ExitCode: 0}, nil
}

// TestExecuteCarriesPrivilegedToDriver proves the marker persisted on the row
// reaches the driver's Spec, the seam the sandboxed drivers read to skip the
// sandbox. A non-privileged spec leaves it off, so ordinary jobs are untouched.
func TestExecuteCarriesPrivilegedToDriver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		privileged bool
	}{{"privileged", true}, {"normal", false}} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })

			drv := &recordingDriver{}
			q := queue.New(st, drv, queue.Options{
				Workers:      1,
				PollInterval: 10 * time.Millisecond,
				Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			})

			spec := shellSpec("/bin/true")
			spec.Privileged = tc.privileged
			if _, err := q.Enqueue(ctx, spec); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			drain(t, q)

			if drv.last.Privileged != tc.privileged {
				t.Errorf("driver Spec.Privileged = %v, want %v", drv.last.Privileged, tc.privileged)
			}
		})
	}
}

func TestDrainTimesOutJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := shellSpec("/bin/sleep", "30")
	spec.Timeout = 150 * time.Millisecond
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	drain(t, q)

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateTimedOut {
		t.Fatalf("state = %s, want timed_out", got.State)
	}
}

func TestEnqueueDeduplicatesOnIdempotencyKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := shellSpec("/bin/echo", "once")
	spec.IdempotencyKey = "webhook-42"

	first, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	second, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-enqueue created job %d, want %d", second.ID, first.ID)
	}

	drain(t, q)

	// A retry after the job finished still resolves to the original job.
	third, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("post-run enqueue: %v", err)
	}
	if third.ID != first.ID {
		t.Fatalf("post-run enqueue created job %d, want %d", third.ID, first.ID)
	}

	jobs, err := st.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("job count = %d, want 1", len(jobs))
	}
}

// markerScript appends "start", sleeps, then appends "end" to path. Serial
// execution therefore produces strictly alternating lines.
func markerScript(path string) []string {
	return []string{"/bin/sh", "-c", "echo start >> " + path + "; sleep 0.2; echo end >> " + path}
}

func readMarkers(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	return strings.Fields(string(data))
}

func TestSharedConcurrencyKeySerializes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, _ := newQueue(t, 4)

	markers := filepath.Join(t.TempDir(), "markers")
	const jobs = 4
	for range jobs {
		spec := shellSpec(markerScript(markers)...)
		spec.ConcurrencyKey = "workspace-a"
		if _, err := q.Enqueue(ctx, spec); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	drain(t, q)

	lines := readMarkers(t, markers)
	if len(lines) != jobs*2 {
		t.Fatalf("markers = %q, want %d lines", lines, jobs*2)
	}
	for i, line := range lines {
		want := "start"
		if i%2 == 1 {
			want = "end"
		}
		if line != want {
			t.Fatalf("markers = %q: line %d is %q, want %q (jobs overlapped)", lines, i, line, want)
		}
	}
}

func TestDistinctConcurrencyKeysOverlap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, _ := newQueue(t, 4)

	markers := filepath.Join(t.TempDir(), "markers")
	for _, key := range []string{"workspace-a", "workspace-b"} {
		spec := shellSpec(markerScript(markers)...)
		spec.ConcurrencyKey = key
		if _, err := q.Enqueue(ctx, spec); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	drain(t, q)

	lines := readMarkers(t, markers)
	if len(lines) != 4 {
		t.Fatalf("markers = %q, want 4 lines", lines)
	}
	if lines[0] != "start" || lines[1] != "start" {
		t.Fatalf("markers = %q, want both jobs started before either ended", lines)
	}
}

func TestRunCancelsInFlightJob(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q, st := newQueue(t, 1)

	job, err := q.Enqueue(context.Background(), shellSpec("/bin/sleep", "30"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- q.Run(ctx) }()

	waitForState(t, st, job.ID, store.StateRunning)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("queue did not stop after cancel")
	}

	got, err := st.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateCancelled {
		t.Fatalf("state = %s, want cancelled", got.State)
	}
}

func TestInterruptedJobsAreNotRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	markers := filepath.Join(t.TempDir(), "markers")
	job, err := q.Enqueue(ctx, shellSpec("/bin/sh", "-c", "echo ran >> "+markers))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// What a boot-time sweep leaves behind.
	if err := st.TransitionJob(ctx, job.ID, store.StateQueued, store.StateInterrupted); err != nil {
		t.Fatalf("interrupt job: %v", err)
	}

	drain(t, q)

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateInterrupted {
		t.Fatalf("state = %s, want interrupted", got.State)
	}
	if _, err := os.Stat(markers); !os.IsNotExist(err) {
		t.Fatal("an interrupted job was executed; interrupted is terminal and never auto-retried")
	}
}

func waitForState(t *testing.T, st *store.Store, id int64, want store.JobState) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		job, err := st.GetJob(context.Background(), id)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %d never reached %s", id, want)
}
