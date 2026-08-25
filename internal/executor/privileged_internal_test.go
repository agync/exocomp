package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// spyDriver records whether it ran and the spec it was handed, so a test can
// assert a privileged run reached the plain inner driver rather than a sandbox.
type spyDriver struct {
	ran  bool
	spec Spec
}

func (d *spyDriver) Run(_ context.Context, spec Spec, _ EventSink) (Result, error) {
	d.ran = true
	d.spec = spec
	return Result{ExitCode: 0}, nil
}

// TestRunUnsandboxedDelegatesWithoutCaps: a privileged run with no caps runs the
// plain driver as-is — that is the whole opt-out.
func TestRunUnsandboxedDelegatesWithoutCaps(t *testing.T) {
	t.Parallel()

	spy := &spyDriver{}
	spec := Spec{Kind: KindCLI, Argv: []string{"tool"}, Privileged: true}
	if _, err := runUnsandboxed(context.Background(), spy, Caps{}, spec, SinkFunc(func(Event) {})); err != nil {
		t.Fatalf("runUnsandboxed: %v", err)
	}
	if !spy.ran {
		t.Fatal("plain driver did not run")
	}
	if !spy.spec.Privileged {
		t.Error("the spec handed to the plain driver lost its Privileged marker")
	}
}

// TestRunUnsandboxedRefusesCaps: caps cannot be enforced without the sandbox, so
// a privileged run that also named a runner is refused rather than run silently
// uncapped (ADR 0005, "never silently").
func TestRunUnsandboxedRefusesCaps(t *testing.T) {
	t.Parallel()

	spy := &spyDriver{}
	caps := Caps{MemoryMaxBytes: 1 << 20}
	_, err := runUnsandboxed(context.Background(), spy, caps, Spec{Privileged: true}, SinkFunc(func(Event) {}))
	if err == nil {
		t.Fatal("a privileged run with caps was not refused")
	}
	if spy.ran {
		t.Error("the plain driver ran despite the caps refusal")
	}
}

// TestSandboxedCLIPrivilegedBypassesSandbox proves the sandboxed CLI driver
// honours Spec.Privileged end to end: a privileged run executes the tool with no
// nono wrapper, so it writes a marker at a path no sandbox profile would grant —
// something a non-privileged run of the same driver never manages (the sandbox
// denies the write, or nono is absent and the wrapped exec fails). The tool is a
// tiny shell script, so the plain path needs no external toolchain.
func TestSandboxedCLIPrivilegedBypassesSandbox(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "escaped")
	tool := filepath.Join(dir, "writer")
	script := "#!/bin/sh\necho reached > \"" + marker + "\"\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatalf("write tool: %v", err)
	}

	newDriver := func() Driver {
		return NewSandboxedCLI(
			CLIOptions{Tool: "writer", Binary: tool, PrivateDirRoot: dir},
			SandboxOptions{DataDir: t.TempDir()},
		)
	}
	spec := func(priv bool) Spec {
		return Spec{Kind: KindCLI, Argv: []string{"writer"}, Privileged: priv}
	}
	sink := SinkFunc(func(Event) {})

	res, err := newDriver().Run(context.Background(), spec(true), sink)
	if err != nil {
		t.Fatalf("privileged run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("privileged run exit = %d, want 0", res.ExitCode)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("privileged run did not run unsandboxed: marker absent: %v", err)
	}

	_ = os.Remove(marker)
	// The non-privileged run takes the sandbox path; the ungranted write never
	// lands, whether nono denies it or is absent so the wrapped exec fails.
	_, _ = newDriver().Run(context.Background(), spec(false), sink)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a non-privileged run reached the ungranted path; the sandbox was skipped")
	}
}
