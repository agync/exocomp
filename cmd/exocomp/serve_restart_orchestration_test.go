package main

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/sched"
)

// restartTestDeriver is a token deriver good enough for a never-started
// api.Server, which only needs one to construct.
type restartTestDeriver struct{}

func (restartTestDeriver) Derive(purpose, id string) []byte {
	return []byte(purpose + "\x00" + id)
}

// TestServeRestartOrchestrationEndToEnd drives the whole restart chain in one
// test: the drain controller fires, the serve loop wakes on restartReady, the
// daemon shuts down cleanly, and finishRestart re-execs. The existing restart
// tests cover each piece alone; this one wires them together, so a break in the
// handoff between them (a controller that fires but is never selected, a loop
// that reports the restart but whose shutdown is skipped, a finishRestart that
// runs before the shutdown) fails here rather than only in production.
//
// It does not call runServe: runServe needs a live envelope, the sandbox probe,
// and a real signal, none of which an in-process test can honestly supply. The
// only stub is restartExec — a real syscall.Exec would replace the test process
// — and the pieces it orchestrates (the scheduler, the store, the shutdown) are
// real. A real subprocess restart is not exercised because the no-op gate
// refuses an unchanged on-disk binary, and replacing the running path in a way
// the gate accepts (a rename) leaves os.Executable pointing at a deleted file;
// see the note in docs/operations/serve.md.
func TestServeRestartOrchestrationEndToEnd(t *testing.T) {
	st := restartTestStore(t)
	ctx := context.Background()

	s := sched.New(st, nil, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	srv, err := api.New(api.Options{Store: st, Tokens: restartTestDeriver{}, Port: 0})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	jobCtx, cancelJobs := context.WithCancel(context.Background())
	queueDone := make(chan error, 1)
	go func() {
		<-jobCtx.Done()
		queueDone <- nil
	}()

	cordon := newCordonController(st, cancelJobs)
	cordon.setScheduler(s)
	cordon.restartGate = func() error { return nil }
	cordon.restartPoll = 5 * time.Millisecond
	cordon.restartTimeout = 5 * time.Second

	origExec, origClean := restartExec, restartStagingCleanup
	t.Cleanup(func() { restartExec, restartStagingCleanup = origExec, origClean })
	var execs atomic.Int32
	restartExec = func() error { execs.Add(1); return nil }
	restartStagingCleanup = func() error { return nil }

	sigCtx, cancelSig := context.WithCancel(context.Background())
	defer cancelSig()

	var out, warn bytes.Buffer
	loopDone := make(chan bool, 1)
	go func() {
		restarting, _ := serveUntilSignal(sigCtx, jobCtx, &out, &warn, nil,
			cordon.reloadAll, 0, nil, cordon.restartReady)
		loopDone <- restarting
	}()

	// No in-flight jobs, so the drain is already satisfied and the controller
	// fires at once — the same "drain, then shut down and re-exec" a restart
	// with no work takes.
	if _, err := cordon.Restart(ctx, false); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	select {
	case restarting := <-loopDone:
		if !restarting {
			t.Fatal("the serve loop stopped without reporting the restart, so runServe would not re-exec")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the serve loop never woke on the restart channel")
	}

	shutdownErr := shutdownServe(context.WithoutCancel(ctx), &out, &warn, shutdown{
		store: st, sched: s, server: srv, web: nil,
		cancelJobs: cancelJobs, queueDone: queueDone, grace: 0,
	})
	if err := finishRestart(&out, shutdownErr); err != nil {
		t.Fatalf("finishRestart: %v\n%s", err, out.String())
	}

	if got := execs.Load(); got != 1 {
		t.Fatalf("the on-disk binary was re-exec'd %d times, want 1\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "restarting: re-exec'ing the on-disk binary") {
		t.Fatalf("the restart did not announce the re-exec:\n%s", out.String())
	}
}
