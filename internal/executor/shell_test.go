package executor_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

type collector struct {
	mu     sync.Mutex
	events []executor.Event
}

func (c *collector) Emit(e executor.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *collector) msgs(level string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if level == "" || e.Level == level {
			out = append(out, e.Msg)
		}
	}
	return out
}

func TestShellRunReportsExitCode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		argv []string
		want int
	}{
		{"true", []string{"/bin/true"}, 0},
		{"false", []string{"/bin/false"}, 1},
		{"exit-7", []string{"/bin/sh", "-c", "exit 7"}, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sink collector
			res, err := executor.NewShell().Run(context.Background(),
				executor.Spec{Argv: tc.argv}, &sink)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.ExitCode != tc.want {
				t.Errorf("exit code = %d, want %d", res.ExitCode, tc.want)
			}
			if res.Signaled || res.TimedOut {
				t.Errorf("result = %+v, want a clean exit", res)
			}
		})
	}
}

func TestShellRunStreamsOutput(t *testing.T) {
	t.Parallel()

	var sink collector
	res, err := executor.NewShell().Run(context.Background(), executor.Spec{
		Argv: []string{"/bin/sh", "-c", "echo one; echo two; echo boom >&2"},
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}

	stdout := sink.msgs(executor.LevelInfo)
	if len(stdout) != 2 || stdout[0] != "one" || stdout[1] != "two" {
		t.Errorf("stdout events = %q", stdout)
	}
	stderr := sink.msgs(executor.LevelError)
	if len(stderr) != 1 || stderr[0] != "boom" {
		t.Errorf("stderr events = %q", stderr)
	}
}

func TestShellRunHonoursDirAndEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var sink collector
	_, err := executor.NewShell().Run(context.Background(), executor.Spec{
		Argv: []string{"/bin/sh", "-c", "pwd; echo $EXOCOMP_TEST"},
		Dir:  dir,
		Env:  []string{"EXOCOMP_TEST=set"},
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	out := sink.msgs(executor.LevelInfo)
	if len(out) != 2 {
		t.Fatalf("stdout events = %q", out)
	}
	// macOS-style symlinked temp dirs would break a raw compare; resolve both.
	got, err := filepath.EvalSymlinks(out[0])
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	if got != want {
		t.Errorf("working dir = %q, want %q", got, want)
	}
	if out[1] != "set" {
		t.Errorf("env passthrough = %q", out[1])
	}
}

func TestShellRunTimesOut(t *testing.T) {
	t.Parallel()

	var sink collector
	start := time.Now()
	res, err := executor.NewShell().Run(context.Background(), executor.Spec{
		Argv:    []string{"/bin/sleep", "30"},
		Timeout: 150 * time.Millisecond,
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("result = %+v, want TimedOut", res)
	}
	if !res.Signaled || res.ExitCode >= 0 {
		t.Errorf("result = %+v, want a signalled process", res)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout took %v", elapsed)
	}
}

func TestShellRunHonoursCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	var sink collector
	res, err := executor.NewShell().Run(ctx, executor.Spec{Argv: []string{"/bin/sleep", "30"}}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.TimedOut {
		t.Errorf("cancelled run reported TimedOut: %+v", res)
	}
	if !res.Signaled {
		t.Errorf("result = %+v, want a signalled process", res)
	}
}

func TestShellRunRejectsBadSpec(t *testing.T) {
	t.Parallel()

	var sink collector
	if _, err := executor.NewShell().Run(context.Background(), executor.Spec{}, &sink); err == nil {
		t.Error("empty argv was accepted")
	}
	_, err := executor.NewShell().Run(context.Background(),
		executor.Spec{Argv: []string{"/nonexistent/exocomp-not-a-binary"}}, &sink)
	if err == nil {
		t.Error("missing executable was accepted")
	}
}
