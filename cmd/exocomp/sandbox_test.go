package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// useDataDir points this process — and every child it starts — at a scratch
// data dir, and pins the plain local tier.
//
// Pinning is deliberate. Under the sandboxed tier a job may write only inside
// its own private directory and the paths its profile names, and the stand-in
// tools these tests install write their invocation logs next to themselves.
// Leaving the tier to the probe would also make the outcome depend on how long
// the test's own name is, because the private root's length is part of what the
// probe decides on. The sandboxed tier has its own end-to-end coverage against
// real ansible and tofu in internal/executor.
func useDataDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv(sandboxTierEnv, string(executor.TierLocal))
}

// TestDoctorReportsTheSandboxTier: doctor names the launcher, the kernel's
// Landlock ABI, the tier in effect, and what the probe asserted — the four
// lines an operator needs to tell an enforced sandbox from a degraded one.
func TestDoctorReportsTheSandboxTier(t *testing.T) {
	resetSandboxDecision(t)

	var out bytes.Buffer
	reportSandbox(context.Background(), &out)

	report := out.String()
	for _, want := range []string{"sandbox:", executor.NonoBinary, "landlock abi", "tier", "probe"} {
		if !strings.Contains(report, want) {
			t.Errorf("doctor's sandbox block does not report %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, executor.NonoMinVersion) {
		t.Errorf("doctor does not state the %s version floor:\n%s", executor.NonoBinary, report)
	}
}

// TestPinnedLocalTierSkipsTheProbe: asking for plain local is a decision, not a
// degradation, and it must not be reported as one.
func TestPinnedLocalTierSkipsTheProbe(t *testing.T) {
	resetSandboxDecision(t)
	t.Setenv(sandboxTierEnv, string(executor.TierLocal))

	res := sandboxTier(context.Background())
	if res.Effective != executor.TierLocal {
		t.Errorf("effective tier = %s, want %s", res.Effective, executor.TierLocal)
	}
	if res.Degraded() {
		t.Error("a pinned local tier is reported as a degradation")
	}
	if res.Enforced {
		t.Error("the probe ran even though no sandbox was asked for")
	}
}

func TestRequestSandboxTierRefusesAnUnknownName(t *testing.T) {
	resetSandboxDecision(t)

	if err := requestSandboxTier("paranoid", nil); err == nil {
		t.Fatal("an unknown tier name was accepted")
	}
}

// resetSandboxDecision clears the once-per-process tier decision so a test can
// make its own, and clears it again afterwards so the next test starts from an
// undecided process the way a real one does.
func resetSandboxDecision(t *testing.T) {
	t.Helper()
	undecide := func() {
		sandbox.once = sync.Once{}
		sandbox.requested = ""
		sandbox.result = executor.ProbeResult{}
		sandbox.warn = io.Discard
	}
	undecide()
	t.Cleanup(undecide)
}

// TestExplicitSandboxFailsClosed: a named local-sandboxed request that the host
// cannot enforce is a refusal, not a silent downgrade — running the fleet's
// jobs unsandboxed is not what was asked for.
func TestExplicitSandboxFailsClosed(t *testing.T) {
	resetSandboxDecision(t)
	sandbox.requested = executor.TierLocalSandboxed
	sandbox.result = executor.ProbeResult{
		Requested: executor.TierLocalSandboxed,
		Effective: executor.TierLocal,
		Reason:    "Landlock unavailable on this kernel",
	}
	sandbox.once.Do(func() {})

	if err := enforceSandboxTier(context.Background()); err == nil {
		t.Fatal("an unenforceable explicit local-sandboxed request was allowed to run")
	}
}

// TestAutoTierDegradesWithoutRefusing: auto means "sandbox where the kernel
// allows it, and say so loudly where it does not", so a degraded auto tier runs.
func TestAutoTierDegradesWithoutRefusing(t *testing.T) {
	resetSandboxDecision(t)
	sandbox.requested = executor.TierAuto
	sandbox.result = executor.ProbeResult{
		Requested: executor.TierAuto,
		Effective: executor.TierLocal,
		Reason:    "Landlock unavailable on this kernel",
	}
	sandbox.once.Do(func() {})

	if err := enforceSandboxTier(context.Background()); err != nil {
		t.Fatalf("auto tier refused to degrade: %v", err)
	}
}
