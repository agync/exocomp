package executor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// grandchildScript starts a long-sleeping grandchild that inherits the job's
// output pipes and records its pid in path, then runs tail in the shell.
func grandchildScript(path, tail string) []string {
	return []string{"/bin/sh", "-c",
		"/bin/sleep 300 & echo $! > " + path + "; " + tail}
}

func grandchildPID(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the script never recorded a grandchild pid in %s", path)
	return 0
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// TestShellRunCancelSignalsTheProcessGroup: a job is the tree its process
// started, not one pid. A cancel that reached only the direct child would
// leave the helpers it spawned running, and their inherited pipes would keep
// the job from ever reaching a terminal state.
func TestShellRunCancelSignalsTheProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow process-group shell test in short mode")
	}
	t.Parallel()

	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		sink   collector
		res    executor.Result
		runErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, runErr = executor.NewShell().Run(ctx, executor.Spec{
			Argv: grandchildScript(pidfile, "exec /bin/sleep 300"),
		}, &sink)
	}()

	pid := grandchildPID(t, pidfile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("a cancelled job never returned")
	}
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if !res.Signaled {
		t.Errorf("result = %+v, want a signalled process", res)
	}

	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Errorf("grandchild %d survived the cancel; the process group was not signalled", pid)
	}
}

// TestShellRunDoesNotWaitOnAnInheritedPipe: the job's own process exited
// cleanly and a helper it left behind holds the output pipes. The job's
// outcome is the exit code it already has — waiting for that helper's EOF made
// the run last as long as the helper did.
func TestShellRunDoesNotWaitOnAnInheritedPipe(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow process-group shell test in short mode")
	}
	t.Parallel()

	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	var (
		sink   collector
		res    executor.Result
		runErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, runErr = executor.NewShell().Run(context.Background(), executor.Spec{
			Argv: grandchildScript(pidfile, "echo parent done; exit 0"),
		}, &sink)
	}()

	pid := grandchildPID(t, pidfile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a finished job never returned while a grandchild held its pipes")
	}
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	// What the job printed before it exited is still reported in full.
	if out := sink.msgs(executor.LevelInfo); len(out) != 1 || out[0] != "parent done" {
		t.Errorf("stdout events = %q, want the line the job printed", out)
	}
}
