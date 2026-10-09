package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/web"
)

// restartTestStore opens a store in a temp dir, the same one a controller's
// in-flight count reads.
func restartTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// runningJobOwnedByThisProcess creates a job and claims it, so it is running
// with owner_pid = this test process — the shape a job that asks for a restart
// has while it is still alive.
func runningJobOwnedByThisProcess(t *testing.T, st *store.Store) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := st.CreateJob(ctx, store.NewJob{
		Runbook: "restart-caller", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := st.ClaimJob(ctx, job.ID); err != nil {
		t.Fatalf("claim job: %v", err)
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return got
}

// newTestRestartController builds a controller whose drain is driven by the
// test's clock and whose binary gate always passes, so the drain logic is
// exercised without a scheduler or a real binary.
func newTestRestartController(t *testing.T, st *store.Store) *cordonController {
	t.Helper()
	c := newCordonController(st, nil)
	c.restartGate = func() error { return nil }
	c.restartPoll = 5 * time.Millisecond
	c.restartTimeout = 5 * time.Second
	return c
}

func restartFired(c *cordonController) bool {
	select {
	case <-c.restartReady:
		return true
	default:
		return false
	}
}

func waitRestartFired(t *testing.T, c *cordonController) {
	t.Helper()
	select {
	case <-c.restartReady:
	case <-time.After(2 * time.Second):
		t.Fatal("the restart action never fired")
	}
}

// TestRestartWaitsForTheCallingJob is the deadlock gate: a job that requests a
// restart is still running and owned by this process, so the restart action must
// not fire until that job is terminal. The request itself returns at once — it
// never blocks the job that made it.
func TestRestartWaitsForTheCallingJob(t *testing.T) {
	st := restartTestStore(t)
	job := runningJobOwnedByThisProcess(t, st)
	c := newTestRestartController(t, st)

	got, err := c.Restart(context.Background(), false)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !got.Pending || got.InFlight != 1 {
		t.Fatalf("Restart returned %+v, want pending with the caller in flight", got)
	}

	// Give the drain goroutine several poll intervals to see the caller and
	// wait; it must not fire while the caller runs.
	time.Sleep(50 * time.Millisecond)
	if restartFired(c) {
		t.Fatal("the restart fired while the requesting job was still running")
	}

	if err := st.TransitionJob(context.Background(), job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("finish caller: %v", err)
	}
	waitRestartFired(t, c)
}

// TestRestartTimeoutAbortsAndUncordons: a drain that cannot finish within its
// bound is a fail-safe abort — the cordon is lifted and the daemon keeps taking
// work, never cancelling the jobs implicitly.
func TestRestartTimeoutAbortsAndUncordons(t *testing.T) {
	st := restartTestStore(t)
	runningJobOwnedByThisProcess(t, st)
	c := newCordonController(st, nil)
	c.restartGate = func() error { return nil }
	c.restartPoll = 5 * time.Millisecond
	c.restartTimeout = 30 * time.Millisecond

	if _, err := c.Restart(context.Background(), false); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := c.RestartStatus(context.Background())
		if err != nil {
			t.Fatalf("RestartStatus: %v", err)
		}
		if !got.Pending {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("the restart never aborted on its timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if restartFired(c) {
		t.Fatal("a timed-out restart fired instead of aborting")
	}
	state, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Cordoned {
		t.Fatal("a timed-out restart left the daemon cordoned")
	}
	c.mu.Lock()
	abortErr := c.restartErr
	c.mu.Unlock()
	if !errors.Is(abortErr, errRestartTimedOut) {
		t.Fatalf("abort reason = %v, want %v", abortErr, errRestartTimedOut)
	}
}

// TestRestartForceCancelsThenRestarts: --force skips the wait, cancels the
// remaining jobs, and fires the restart.
func TestRestartForceCancelsThenRestarts(t *testing.T) {
	st := restartTestStore(t)
	runningJobOwnedByThisProcess(t, st)
	c := newCordonController(st, nil)
	c.restartGate = func() error { return nil }
	c.restartPoll = 5 * time.Millisecond
	c.restartTimeout = time.Minute

	var cancelled atomic.Bool
	c.restartCancel = func() { cancelled.Store(true) }

	got, err := c.Restart(context.Background(), true)
	if err != nil {
		t.Fatalf("Restart --force: %v", err)
	}
	if !got.Pending {
		t.Fatalf("Restart --force returned %+v, want pending", got)
	}
	waitRestartFired(t, c)
	if !cancelled.Load() {
		t.Fatal("--force did not cancel the remaining jobs before restarting")
	}
}

// TestRestartRefusesNoOp: an unchanged on-disk binary is refused and the daemon
// is left uncordoned.
func TestRestartRefusesNoOp(t *testing.T) {
	st := restartTestStore(t)
	c := newCordonController(st, nil)
	c.restartGate = func() error { return web.ErrNothingToRestart }

	_, err := c.Restart(context.Background(), false)
	if !errors.Is(err, web.ErrNothingToRestart) {
		t.Fatalf("Restart error = %v, want ErrNothingToRestart", err)
	}
	if restartFired(c) {
		t.Fatal("a refused restart fired")
	}
	state, _ := c.State(context.Background())
	if state.Cordoned {
		t.Fatal("a refused restart left the daemon cordoned")
	}
}

// TestRestartPrecheckFailureKeepsRunning: a binary that fails its precheck is
// refused, the running process stays up, and the cordon is lifted.
func TestRestartPrecheckFailureKeepsRunning(t *testing.T) {
	st := restartTestStore(t)
	c := newCordonController(st, nil)
	c.restartGate = func() error { return errors.New("on-disk binary failed its precheck") }

	_, err := c.Restart(context.Background(), false)
	if err == nil {
		t.Fatal("a failed precheck did not refuse the restart")
	}
	if restartFired(c) {
		t.Fatal("a restart fired despite a failed precheck")
	}
	state, _ := c.State(context.Background())
	if state.Cordoned {
		t.Fatal("a failed precheck left the daemon cordoned")
	}
}

// TestRestartSecondRequestIsNoOp: while a restart is pending, a second request
// is answered with the pending state and does not re-run the gate or start a
// second drain.
func TestRestartSecondRequestIsNoOp(t *testing.T) {
	st := restartTestStore(t)
	runningJobOwnedByThisProcess(t, st)
	c := newCordonController(st, nil)
	var gateCalls atomic.Int32
	c.restartGate = func() error { gateCalls.Add(1); return nil }
	c.restartPoll = 5 * time.Millisecond
	c.restartTimeout = 200 * time.Millisecond

	first, err := c.Restart(context.Background(), false)
	if err != nil {
		t.Fatalf("first Restart: %v", err)
	}
	second, err := c.Restart(context.Background(), false)
	if err != nil {
		t.Fatalf("second Restart: %v", err)
	}
	if !first.Pending || !second.Pending {
		t.Fatalf("both requests should report pending: first %+v, second %+v", first, second)
	}
	if first.Deadline != second.Deadline {
		t.Fatalf("second request reset the deadline: %v then %v", first.Deadline, second.Deadline)
	}
	if got := gateCalls.Load(); got != 1 {
		t.Fatalf("gate ran %d times, want 1 (the second request is a no-op)", got)
	}
}

// TestFinishRestartAbortsOnShutdownError: a shutdown that errored must not be
// followed by an exec into an unclean state.
func TestFinishRestartAbortsOnShutdownError(t *testing.T) {
	orig := restartExec
	t.Cleanup(func() { restartExec = orig })

	var execs atomic.Int32
	restartExec = func() error { execs.Add(1); return nil }

	shutdownErr := errors.New("shutdown did not complete")
	if got := finishRestart(io.Discard, shutdownErr); !errors.Is(got, shutdownErr) {
		t.Fatalf("finishRestart returned %v, want the shutdown error", got)
	}
	if execs.Load() != 0 {
		t.Fatal("finishRestart exec'd after a failed shutdown")
	}

	if err := finishRestart(io.Discard, nil); err != nil {
		t.Fatalf("finishRestart with a clean shutdown: %v", err)
	}
	if execs.Load() != 1 {
		t.Fatalf("clean shutdown exec'd %d times, want 1", execs.Load())
	}
}

// TestDefaultRestartGateRefusesUnchangedBinary exercises the real gate: the
// test process's on-disk path is the image it is running, so the no-op check
// must refuse.
func TestDefaultRestartGateRefusesUnchangedBinary(t *testing.T) {
	if err := defaultRestartGate(); !errors.Is(err, web.ErrNothingToRestart) {
		t.Fatalf("defaultRestartGate on an unchanged image = %v, want ErrNothingToRestart", err)
	}
}

// TestPrecheckBinary: a binary that answers `version` passes; one that exits
// non-zero is refused, so a broken replacement never gets exec'd.
func TestPrecheckBinary(t *testing.T) {
	good := filepath.Join(t.TempDir(), "good")
	writeFile(t, good, "#!/bin/sh\necho exocomp test\n")
	if err := os.Chmod(good, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := precheckBinary(good); err != nil {
		t.Fatalf("precheckBinary on a working binary: %v", err)
	}

	bad := filepath.Join(t.TempDir(), "bad")
	writeFile(t, bad, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(bad, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := precheckBinary(bad); err == nil {
		t.Fatal("precheckBinary accepted a binary that failed `version`")
	}
}

// TestServeUntilSignalReturnsRestart: the serve loop wakes on the restart
// channel and reports the restart outcome, so runServe can re-exec.
func TestServeUntilSignalReturnsRestart(t *testing.T) {
	st := restartTestStore(t)
	s := sched.New(st, nil, nil)

	sigCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	restart := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		restarting, _ := serveUntilSignal(sigCtx, context.Background(), io.Discard, io.Discard,
			nil, s, 0, nil, restart)
		done <- restarting
	}()

	close(restart)
	select {
	case got := <-done:
		if !got {
			t.Fatal("serveUntilSignal did not report the restart")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveUntilSignal did not return after the restart fired")
	}
}

// TestRestartConcurrentSecondRequestIsNoOp: the pending check and set are
// atomic across the gate, so a second request that arrives while the first is
// still prechecking sees the reservation and returns the current status instead
// of running the gate again.
func TestRestartConcurrentSecondRequestIsNoOp(t *testing.T) {
	st := restartTestStore(t)
	runningJobOwnedByThisProcess(t, st)
	c := newCordonController(st, nil)

	var gateCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	c.restartGate = func() error {
		if gateCalls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}
	c.restartPoll = 5 * time.Millisecond
	c.restartTimeout = time.Minute

	firstDone := make(chan error, 1)
	go func() {
		_, err := c.Restart(context.Background(), false)
		firstDone <- err
	}()

	<-entered
	second, err := c.Restart(context.Background(), false)
	if err != nil {
		t.Fatalf("second Restart: %v", err)
	}
	if !second.Pending {
		t.Fatalf("second Restart = %+v, want it to report the pending restart", second)
	}
	if got := gateCalls.Load(); got != 1 {
		t.Fatalf("gate ran %d times, want 1 (the concurrent second request is a no-op)", got)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Restart: %v", err)
	}
}

// TestRestartGateFailureClearsPending: a refused restart releases the pending
// reservation, so a later request starts clean rather than being told a restart
// is already pending.
func TestRestartGateFailureClearsPending(t *testing.T) {
	st := restartTestStore(t)
	c := newCordonController(st, nil)
	c.restartGate = func() error { return errors.New("precheck refused") }

	if _, err := c.Restart(context.Background(), false); err == nil {
		t.Fatal("a refused gate did not fail the restart")
	}
	c.mu.Lock()
	pending := c.restartPending
	c.mu.Unlock()
	if pending {
		t.Fatal("a refused restart left restartPending set")
	}
}

// TestFinishRestartClosesStagingBeforeExec: the run-secret staging session dir
// is removed before the re-exec (the exec skips runServe's deferred close), and
// a shutdown error skips both the cleanup and the exec.
func TestFinishRestartClosesStagingBeforeExec(t *testing.T) {
	origExec, origClean := restartExec, restartStagingCleanup
	t.Cleanup(func() { restartExec, restartStagingCleanup = origExec, origClean })

	var order []string
	restartStagingCleanup = func() error { order = append(order, "cleanup"); return nil }
	restartExec = func() error { order = append(order, "exec"); return nil }

	if err := finishRestart(io.Discard, nil); err != nil {
		t.Fatalf("finishRestart: %v", err)
	}
	if len(order) != 2 || order[0] != "cleanup" || order[1] != "exec" {
		t.Fatalf("order = %v, want cleanup before exec", order)
	}

	order = nil
	if err := finishRestart(io.Discard, errors.New("shutdown did not complete")); err == nil {
		t.Fatal("finishRestart accepted a failed shutdown")
	}
	if len(order) != 0 {
		t.Fatalf("cleanup/exec ran after a failed shutdown: %v", order)
	}
}

// TestDefaultRestartGateRefusesUnresolvablePath: a path that cannot be resolved
// is a refusal, not a skip — a restart must not proceed on an unverified binary.
func TestDefaultRestartGateRefusesUnresolvablePath(t *testing.T) {
	orig := onDiskBinaryPathFn
	t.Cleanup(func() { onDiskBinaryPathFn = orig })
	onDiskBinaryPathFn = func() (string, error) { return "", errors.New("no /proc/self/exe") }

	if err := defaultRestartGate(); err == nil {
		t.Fatal("defaultRestartGate proceeded without a resolvable on-disk binary")
	}
}

// TestCordonSetSchedulerRace hammers the scheduler-handle write against the
// Cordon/Uncordon/Reload reads. It asserts nothing beyond not tripping the race
// detector, which is the whole point: the write must be under the same lock as
// the reads.
func TestCordonSetSchedulerRace(t *testing.T) {
	st := restartTestStore(t)
	c := newCordonController(st, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.setScheduler(nil)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = c.Cordon(context.Background())
				_, _ = c.Uncordon(context.Background())
				_, _ = c.Reload(context.Background())
			}
		}()
	}
	wg.Wait()
}
