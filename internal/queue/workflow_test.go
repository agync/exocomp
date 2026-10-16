package queue

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

func readMarkers(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	return strings.Fields(string(data))
}

func readMarkerFile(path string) ([]byte, error) { return os.ReadFile(path) }

func writeFile(path string) error { return os.WriteFile(path, []byte("go"), 0o600) }

// The sequencer lives in package queue (not queue_test) so its resume path can
// be driven against a queue rebuilt over the same store file — the in-process
// stand-in for a daemon restart.

func newSeqQueue(t *testing.T, path string) (*Queue, *store.Store) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q := New(st, executor.NewShell(), Options{
		Workers:      2,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return q, st
}

func step(kind string, argv ...string) Step {
	return Step{Kind: kind, Spec: Spec{Runbook: "wf", Kind: "shell", Trigger: "workflow", Argv: argv}}
}

func runDaemon(t *testing.T, q *Queue) (context.Context, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Run(ctx) }()
	return ctx, cancel, done
}

func stopDaemon(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("daemon run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func mustSteps(t *testing.T, st *store.Store, id string) []store.WorkflowStep {
	t.Helper()
	steps, err := st.GetWorkflow(context.Background(), id)
	if err != nil {
		t.Fatalf("get workflow %s: %v", id, err)
	}
	return steps
}

// TestSequencerRunsSecondStepOnlyAfterFirstSucceeds is the primary gate: step 2
// runs iff step 1 succeeded, and its effect lands only after step 1's.
func TestSequencerRunsSecondStepOnlyAfterFirstSucceeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q, st := newSeqQueue(t, filepath.Join(dir, "exocomp.db"))
	seq := NewSequencer(q)
	ctx := context.Background()

	markers := filepath.Join(dir, "markers")
	// Each step appends its label; ordering in the file proves the sequencing.
	steps := []Step{
		step("shell", "/bin/sh", "-c", "echo one >> "+markers),
		step("shell", "/bin/sh", "-c", "echo two >> "+markers),
	}
	if err := seq.Start(ctx, "wf-a", steps); err != nil {
		t.Fatalf("start: %v", err)
	}

	_, cancel, done := runDaemon(t, q)
	defer stopDaemon(t, cancel, done)

	if err := seq.Run(ctx, "wf-a"); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := readMarkers(t, markers)
	want := []string{"one", "two"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("markers = %v, want %v", got, want)
	}

	ledger := mustSteps(t, st, "wf-a")
	for _, s := range ledger {
		if s.Status != store.StepSucceeded {
			t.Errorf("step %d is %s, want succeeded", s.StepNo, s.Status)
		}
	}
	if got := store.WorkflowStatus(ledger); got != store.WorkflowSucceeded {
		t.Errorf("workflow status = %s, want succeeded", got)
	}
}

// TestSequencerBlocksAfterAFailure proves a failed step leaves the later steps
// blocked and the workflow failed, both readable from the ledger, and the
// blocked step's effect never runs.
func TestSequencerBlocksAfterAFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q, st := newSeqQueue(t, filepath.Join(dir, "exocomp.db"))
	seq := NewSequencer(q)
	ctx := context.Background()

	marker := filepath.Join(dir, "step2-ran")
	steps := []Step{
		step("shell", "/bin/false"), // fails
		step("shell", "/usr/bin/touch", marker),
	}
	if err := seq.Start(ctx, "wf-b", steps); err != nil {
		t.Fatalf("start: %v", err)
	}

	_, cancel, done := runDaemon(t, q)
	defer stopDaemon(t, cancel, done)

	if err := seq.Run(ctx, "wf-b"); err != nil {
		t.Fatalf("run: %v", err)
	}

	ledger := mustSteps(t, st, "wf-b")
	if ledger[0].Status != store.StepFailed {
		t.Errorf("step 1 is %s, want failed", ledger[0].Status)
	}
	if ledger[1].Status != store.StepBlocked {
		t.Errorf("step 2 is %s, want blocked", ledger[1].Status)
	}
	if got := store.WorkflowStatus(ledger); got != store.WorkflowFailed {
		t.Errorf("workflow status = %s, want failed", got)
	}
	if _, err := readMarkerFile(marker); err == nil {
		t.Fatal("step 2 ran though step 1 failed: its marker exists")
	}

	// The blocked step's job was withdrawn-equivalent: it stays awaiting, never
	// claimed, so a running daemon cannot pick it up later.
	step2, err := st.JobByIdempotencyKey(ctx, stepKey("wf-b", 2))
	if err != nil {
		t.Fatalf("find step 2 job: %v", err)
	}
	if step2.State != store.StateAwaitingApproval {
		t.Errorf("step 2 job is %s, want it left un-runnable at awaiting_approval", step2.State)
	}
}

// TestSequencerResumesFromLedgerAfterRestart is the daemon-restart gate: a
// workflow is killed after step 1 succeeds; a fresh queue+sequencer over the
// same database resumes it, runs step 2 from the ledger, and NEVER re-runs step
// 1 — proven by step 1's marker appearing exactly once.
//
// Mutation check: making ResumeAll a no-op fails this at "post-resume workflow
// status = running, want succeeded"; the final [one two] marker assertion is
// what a resume that re-ran step 1 (an extra "one") would trip.
func TestSequencerResumesFromLedgerAfterRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "exocomp.db")
	markers := filepath.Join(dir, "markers")

	// A gate file makes step 2 block until we allow it, so we can kill the
	// daemon in the window after step 1 has succeeded and before step 2 runs.
	gate := filepath.Join(dir, "gate")
	steps := []Step{
		step("shell", "/bin/sh", "-c", "echo one >> "+markers),
		step("shell", "/bin/sh", "-c", "until [ -f "+gate+" ]; do sleep 0.02; done; echo two >> "+markers),
	}

	ctx := context.Background()

	// First process: run until step 1 has succeeded, then kill mid-workflow.
	{
		q, st := newSeqQueue(t, dbPath)
		seq := NewSequencer(q)
		if err := seq.Start(ctx, "wf-c", steps); err != nil {
			t.Fatalf("start: %v", err)
		}
		_, cancel, done := runDaemon(t, q)

		seqCtx, seqCancel := context.WithCancel(ctx)
		seqDone := make(chan error, 1)
		go func() { seqDone <- seq.Run(seqCtx, "wf-c") }()

		// Wait until step 1 is recorded succeeded in the ledger.
		waitForStep(t, st, "wf-c", 1, store.StepSucceeded)

		// Kill the sequencer and the daemon — the crash.
		seqCancel()
		if err := <-seqDone; err != nil {
			t.Logf("sequencer exited after cancel: %v", err)
		}
		stopDaemon(t, cancel, done)

		ledger := mustSteps(t, st, "wf-c")
		if ledger[0].Status != store.StepSucceeded {
			t.Fatalf("pre-restart step 1 is %s, want succeeded", ledger[0].Status)
		}
		if ledger[1].Status == store.StepSucceeded {
			t.Fatalf("pre-restart step 2 already succeeded; the kill window was missed")
		}
	}

	// Let step 2 proceed once it is (re)started.
	if err := writeFile(gate); err != nil {
		t.Fatalf("open gate: %v", err)
	}

	// Second process: a fresh queue+sequencer over the same database resumes.
	{
		q, st := newSeqQueue(t, dbPath)
		seq := NewSequencer(q)

		// The boot job reconcile retires the rows the dead process left, exactly
		// as openJobStore does at daemon start.
		if _, err := st.ReconcileNonTerminal(ctx); err != nil {
			t.Fatalf("boot sweep: %v", err)
		}

		_, cancel, done := runDaemon(t, q)
		defer stopDaemon(t, cancel, done)

		resumed, err := seq.ResumeAll(ctx)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if resumed != 1 {
			t.Fatalf("resumed %d workflows, want 1", resumed)
		}

		ledger := mustSteps(t, st, "wf-c")
		if got := store.WorkflowStatus(ledger); got != store.WorkflowSucceeded {
			t.Fatalf("post-resume workflow status = %s, want succeeded", got)
		}
	}

	// Step 1 ran exactly once (not re-run on resume); step 2 ran once.
	got := readMarkers(t, markers)
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("markers = %v, want [one two] — step 1 re-run would repeat 'one'", got)
	}
}

// degradedStepDriver reports a tolerated degradation for every run, standing in
// for the ansible driver when a TolerateUnreachable play is a workflow step
// (cmd/exocomp/run.go composes exactly that into a gated ledger workflow).
type degradedStepDriver struct{}

func (degradedStepDriver) Run(_ context.Context, _ executor.Spec, _ executor.EventSink) (executor.Result, error) {
	return executor.Result{ExitCode: 4, Degraded: true, UnreachableHosts: []string{"laptop"}}, nil
}

// TestSequencerAdvancesPastADegradedStep proves a step whose job settles
// degraded is a SUCCESS for the chain: a TolerateUnreachable play that
// converged its reachable hosts is not a caller failure (the same judgment the
// CLI exit mappers apply), so it must advance the ledger and release the next
// step instead of blocking it the way a real failure would.
//
// The driver settles EVERY step's job degraded (it never executes argv), so a
// released step proves itself by reaching a terminal job state and a
// StepSucceeded ledger row — the ledger rows and the second job's settled
// state are the whole proof that nothing was blocked.
//
// Mutation check: reverting recordOutcome's degraded admission to
// StateSucceeded-only settles step 1 StepFailed, blocks step 2 (its job stays
// awaiting_approval), and marks the workflow failed — this test then fails on
// every assertion.
func TestSequencerAdvancesPastADegradedStep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "exocomp.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q := New(st, degradedStepDriver{}, Options{
		Workers:      2,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	seq := NewSequencer(q)
	ctx := context.Background()

	steps := []Step{
		step("shell", "/bin/true"),
		step("shell", "/bin/true"),
	}
	if err := seq.Start(ctx, "wf-degraded", steps); err != nil {
		t.Fatalf("start: %v", err)
	}

	_, cancel, done := runDaemon(t, q)
	defer stopDaemon(t, cancel, done)

	if err := seq.Run(ctx, "wf-degraded"); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Step 1's own job really settled degraded — the test is not vacuous.
	step1, err := st.JobByIdempotencyKey(ctx, stepKey("wf-degraded", 1))
	if err != nil {
		t.Fatalf("find step 1 job: %v", err)
	}
	if step1.State != store.StateDegraded {
		t.Errorf("step 1 job is %s, want degraded", step1.State)
	}

	// Step 2 was released and ran (its job settled), not left awaiting behind a
	// block — the exact shape a blocked step would take.
	step2, err := st.JobByIdempotencyKey(ctx, stepKey("wf-degraded", 2))
	if err != nil {
		t.Fatalf("find step 2 job: %v", err)
	}
	if step2.State != store.StateDegraded {
		t.Errorf("step 2 job is %s, want it released and settled (not awaiting_approval behind a block)", step2.State)
	}

	ledger := mustSteps(t, st, "wf-degraded")
	if ledger[0].Status != store.StepSucceeded {
		t.Errorf("step 1 is %s, want succeeded (a degraded play is not a failure)", ledger[0].Status)
	}
	if ledger[1].Status != store.StepSucceeded {
		t.Errorf("step 2 is %s, want succeeded — a degraded step must not block the chain", ledger[1].Status)
	}
	if got := store.WorkflowStatus(ledger); got != store.WorkflowSucceeded {
		t.Errorf("workflow status = %s, want succeeded", got)
	}
}

// TestSequencerRearmsStepInterruptedWhileQueued is the reachability-and-safety
// proof for the info-level finding that ReconcileNonTerminal's owner-liveness
// check might retire a legitimately-queued job whose enqueuing CLI exited: it
// is reachable (a step Release'd into queued keeps the enqueuer's pid as
// owner_pid until something claims it, so a reconcile that runs first retires
// it), but it is not a hazard for a composed chain, because the ledger still
// shows the step unfinished and ResumeAll's rearmStep re-enqueues it under a
// fresh job before anything is lost.
//
// Mutation check: reverting workflow.go's nonDecision `case
// store.StateInterrupted: return true` to false makes ResumeAll settle the
// interrupted job as a decided (failed) step instead of re-arming it — this
// test then fails at "workflow status = failed, want succeeded".
func TestSequencerRearmsStepInterruptedWhileQueued(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "exocomp.db")
	marker := filepath.Join(dir, "ran")

	q, st := newSeqQueue(t, dbPath)
	seq := NewSequencer(q)
	ctx := context.Background()

	steps := []Step{step("shell", "/usr/bin/touch", marker)}
	if err := seq.Start(ctx, "wf-heal", steps); err != nil {
		t.Fatalf("start: %v", err)
	}

	job, err := st.JobByIdempotencyKey(ctx, stepKey("wf-heal", 1))
	if err != nil {
		t.Fatalf("find step job: %v", err)
	}
	// Release it into queued and mark the ledger running, exactly as runStep
	// would, but do not wait on it: no daemon is running, so nothing claims
	// this row yet.
	if err := q.Release(ctx, job.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := st.StartStep(ctx, "wf-heal", 1, job.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}
	if got, err := st.GetJob(ctx, job.ID); err != nil || got.State != store.StateQueued {
		t.Fatalf("job state = %v, %v, want queued", got.State, err)
	}

	// Simulate the enqueuing CLI dying before any daemon claims the row: stamp
	// owner_pid to a pid that ran and already exited, as a reaped killed CLI's
	// pid would read.
	orphanJobOwner(t, dbPath, job.ID)

	// The reconcile sweep the daemon runs on a schedule (and at boot/shutdown)
	// finds the row's owner gone and retires it. No daemon is running yet, so
	// this is not a race the reconcile happens to win — it is the worst case,
	// where it always would.
	rec, err := st.ReconcileNonTerminal(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Interrupted != 1 {
		t.Fatalf("reconcile = %+v, want 1 interrupted", rec)
	}
	if got, err := st.GetJob(ctx, job.ID); err != nil || got.State != store.StateInterrupted {
		t.Fatalf("job state = %v, %v, want interrupted", got.State, err)
	}

	// Now the daemon comes up and drives the ledger, exactly as
	// cmd/exocomp/serve.go's driveWorkflows loop does every second.
	_, cancel, done := runDaemon(t, q)
	defer stopDaemon(t, cancel, done)

	resumed, err := seq.ResumeAll(ctx)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed != 1 {
		t.Fatalf("resumed %d workflows, want 1", resumed)
	}

	waitForStep(t, st, "wf-heal", 1, store.StepSucceeded)

	ledger := mustSteps(t, st, "wf-heal")
	if got := store.WorkflowStatus(ledger); got != store.WorkflowSucceeded {
		t.Fatalf("workflow status = %s, want succeeded (ledger: %+v)", got, ledger)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("step never ran: %v", err)
	}

	// The original interrupted row is left untouched as history; the step that
	// actually ran is a different job the rearm created.
	original, err := st.GetJob(ctx, job.ID)
	if err != nil || original.State != store.StateInterrupted {
		t.Fatalf("original job state = %v, %v, want left interrupted", original.State, err)
	}
	if ledger[0].JobID == nil || *ledger[0].JobID == job.ID {
		t.Fatalf("ledger step job = %v, want a fresh id distinct from interrupted job %d",
			ledger[0].JobID, job.ID)
	}
}

// orphanJobOwner rewrites a job's owner_pid to one that ran and already
// exited, simulating a CLI killed after it enqueued or released the row. The
// store package's own SetOwnerPIDForTest is test-only and not linked into
// other packages' test binaries, so this opens a second connection to the
// same file and performs the same write directly.
func orphanJobOwner(t *testing.T, dbPath string, jobID int64) {
	t.Helper()
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run /bin/true: %v", err)
	}
	dead := cmd.Process.Pid

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE job SET owner_pid = ? WHERE id = ?`, dead, jobID); err != nil {
		t.Fatalf("orphan job owner: %v", err)
	}
}

func waitForStep(t *testing.T, st *store.Store, id string, stepNo int, want store.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s, err := st.GetWorkflowStep(context.Background(), id, stepNo)
		if err == nil && s.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("workflow %s step %d never reached %s", id, stepNo, want)
}
