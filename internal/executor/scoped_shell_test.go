package executor_test

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// The reactive engine's shell action (and a scheduler's shell fire) run an
// operator-configured command through the same "rest" driver the daemon
// wires up in production. A bare Shell.Run with no explicit spec.Env
// inherits the daemon's own full process environment (Go's exec.Cmd default
// for a nil Env) — including anything sensitive exported to exocompd itself,
// which an operator's rule was never meant to read.
func TestScopedShellDoesNotLeakDaemonEnv(t *testing.T) {
	t.Setenv("EXOCOMP_DAEMON_SENTINEL", "leaked-if-you-see-this")

	var sink collector
	_, err := executor.NewScopedShell().Run(context.Background(), executor.Spec{
		Argv: []string{"/bin/sh", "-c", "echo \"[$EXOCOMP_DAEMON_SENTINEL]\""},
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	out := sink.msgs(executor.LevelInfo)
	if len(out) != 1 {
		t.Fatalf("stdout events = %q", out)
	}
	if out[0] != "[]" {
		t.Fatalf("shell action saw the daemon's env: %q", out[0])
	}
}

// A caller that DOES set an explicit spec.Env (a governed driver's own
// curated env) is passed through unchanged — the scoping only fills in a
// default when nothing was set.
func TestScopedShellHonoursExplicitEnv(t *testing.T) {
	var sink collector
	_, err := executor.NewScopedShell().Run(context.Background(), executor.Spec{
		Argv: []string{"/bin/sh", "-c", "echo $EXOCOMP_TEST"},
		Env:  []string{"EXOCOMP_TEST=set"},
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	out := sink.msgs(executor.LevelInfo)
	if len(out) != 1 || out[0] != "set" {
		t.Fatalf("stdout events = %q, want [set]", out)
	}
}
