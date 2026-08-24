package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
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

// TestDoctorReportsRunSecretFileStaging proves the doctor names the resolved
// backend, where it lands, and the mlock posture — the operator-facing
// surface of the staging path. In this environment /dev/shm is real,
// unusable-sized tmpfs, so the auto default resolves to it with no fallback.
func TestDoctorReportsRunSecretFileStaging(t *testing.T) {
	var out bytes.Buffer
	printRunSecretFileStaging(&out, t.TempDir(), executor.StagingAuto)
	report := out.String()
	if !strings.Contains(report, "run-secret file staging:") {
		t.Errorf("doctor did not report the run-secret file staging block:\n%s", report)
	}
	if !strings.Contains(report, executor.TmpfsRoot) {
		t.Errorf("doctor did not name %s:\n%s", executor.TmpfsRoot, report)
	}
	if !strings.Contains(report, "memlock") {
		t.Errorf("doctor did not report the memlock posture:\n%s", report)
	}
	if !strings.Contains(report, string(executor.StagingAuto)) {
		t.Errorf("doctor did not name the configured backend:\n%s", report)
	}
}

// TestDoctorReportsDiskBackendResidual proves an explicit disk backend is
// reported as resolved to the disk root with its residual posture named,
// rather than silently reading like the tmpfs default.
func TestDoctorReportsDiskBackendResidual(t *testing.T) {
	var out bytes.Buffer
	dataDir := t.TempDir()
	printRunSecretFileStaging(&out, dataDir, executor.StagingDisk)
	report := out.String()
	if !strings.Contains(report, string(executor.StagingDisk)) {
		t.Errorf("doctor did not name the disk backend:\n%s", report)
	}
	if !strings.Contains(report, "disk residual") {
		t.Errorf("doctor did not report the disk residual posture:\n%s", report)
	}
}

// TestConfiguredSecretStagingBackendValidatesPersistedValue proves the
// doctor's read-back of the persisted secret-staging setting cannot resolve
// to the disk backend just because a stored value is unrecognized — the
// only way to get the disk backend is to have actually persisted "disk".
// An unrecognized or empty stored value must resolve to the same safe
// default (StagingAuto) a fresh, never-configured data dir gets.
func TestConfiguredSecretStagingBackendValidatesPersistedValue(t *testing.T) {
	cases := []struct {
		name     string
		stored   string
		write    bool
		want     executor.StagingBackend
		wantWarn bool
	}{
		{name: "unrecognized value", stored: "garbage", write: true, want: executor.StagingAuto, wantWarn: true},
		{name: "empty value", stored: "", write: true, want: executor.StagingAuto, wantWarn: false},
		{name: "auto", stored: "auto", write: true, want: executor.StagingAuto, wantWarn: false},
		{name: "tmpfs", stored: "tmpfs", write: true, want: executor.StagingTmpfs, wantWarn: false},
		{name: "disk", stored: "disk", write: true, want: executor.StagingDisk, wantWarn: false},
		{name: "never written", write: false, want: executor.StagingAuto, wantWarn: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_DATA_HOME", dir)
			dd, err := dataDir()
			if err != nil {
				t.Fatalf("dataDir: %v", err)
			}
			st, err := openStore()
			if err != nil {
				t.Fatalf("openStore: %v", err)
			}
			if tc.write {
				if err := st.PutSetting(context.Background(), store.SettingSecretStaging, tc.stored); err != nil {
					t.Fatalf("PutSetting: %v", err)
				}
			}
			if err := st.Close(); err != nil {
				t.Fatalf("close store: %v", err)
			}

			got, warn := configuredSecretStagingBackend(dd)
			if got != tc.want {
				t.Errorf("configuredSecretStagingBackend() = %s, want %s", got, tc.want)
			}
			if got == executor.StagingDisk && tc.stored != "disk" {
				t.Fatalf("stored value %q resolved to the disk backend", tc.stored)
			}
			if tc.wantWarn && warn == "" {
				t.Errorf("expected a warning for unrecognized stored value %q, got none", tc.stored)
			}
			if !tc.wantWarn && warn != "" {
				t.Errorf("unexpected warning for stored value %q: %s", tc.stored, warn)
			}
		})
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
