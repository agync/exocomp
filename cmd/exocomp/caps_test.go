package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// This file proves a runner's caps configuration is enforced end to end: its
// memory_max/cpu_quota now reaches executor.SandboxOptions.Caps at job
// dispatch (resolveRunnerCaps), and a job that asks for caps but cannot get
// them enforced is refused rather than silently run uncapped, on both axes
// applyJobCaps's fail-closed contract depends on:
//
//   - no delegated cgroup subtree (capsController/withCaps, tested below
//     against a faked cgroup tree so the real host cgroup is never touched)
//   - the sandboxed tier itself not active (requireSandboxedForCaps: a plain
//     subprocess never looks at SandboxOptions.Caps at all)
//
// What happens once a non-empty Caps and a non-nil Cgroup reach
// internal/executor.applyJobCaps — the actual cgroup v2 writes, the
// clone3 CLONE_INTO_CGROUP placement, the OOM/teardown accounting — is
// covered by internal/executor's own tests (cgroup_internal_test.go,
// sandbox_drivers tests); this file is the seam between "an operator named a
// runner with caps" and "applyJobCaps sees them", which is exactly what was
// unreachable before this change.

func testRunnerStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestResolveRunnerCapsEmptyNameIsNoop(t *testing.T) {
	st := testRunnerStore(t)
	caps, err := resolveRunnerCaps(context.Background(), st, "")
	if err != nil {
		t.Fatalf("resolveRunnerCaps(\"\") = %v, want nil", err)
	}
	if !caps.Empty() {
		t.Errorf("resolveRunnerCaps(\"\") = %+v, want Empty()", caps)
	}
}

// TestResolveRunnerCapsFromRegisteredRunner is the RED-before/GREEN-after case
// for the wiring gap itself: a runner's stored memory_max now produces a
// non-empty executor.Caps, where before this change nothing in cmd/exocomp
// ever called ParseCaps on a runner row at all.
func TestResolveRunnerCapsFromRegisteredRunner(t *testing.T) {
	st := testRunnerStore(t)
	ctx := context.Background()
	if _, err := st.CreateRunner(ctx, store.Runner{
		Name: "capped", Mode: store.RunnerModeCached, Base: "fedora-minimal:44",
		MemoryMax: "512M", CPUQuota: "50%",
	}); err != nil {
		t.Fatalf("CreateRunner: %v", err)
	}

	caps, err := resolveRunnerCaps(ctx, st, "capped")
	if err != nil {
		t.Fatalf("resolveRunnerCaps: %v", err)
	}
	want, err := executor.ParseCaps("512M", "50%")
	if err != nil {
		t.Fatalf("ParseCaps: %v", err)
	}
	if caps != want {
		t.Errorf("resolveRunnerCaps = %+v, want %+v", caps, want)
	}
	if caps.Empty() {
		t.Error("a runner with memory_max set produced Empty() caps")
	}
}

// TestResolveRunnerCapsRefusesUnknownRunner: naming a runner that does not
// exist must fail the job, not silently run uncapped as if --runner had
// never been passed.
func TestResolveRunnerCapsRefusesUnknownRunner(t *testing.T) {
	st := testRunnerStore(t)
	if _, err := resolveRunnerCaps(context.Background(), st, "no-such-runner"); err == nil {
		t.Fatal("resolveRunnerCaps accepted an unregistered runner name")
	}
}

// TestResolveRunnerCapsRefusesMalformedCaps: the store layer (unlike the
// `runner add`/`runner import` CLI paths) does not itself validate
// memory_max/cpu_quota, so a row can carry a value ParseCaps rejects. That
// must refuse the job at resolution time, not drop the request.
func TestResolveRunnerCapsRefusesMalformedCaps(t *testing.T) {
	st := testRunnerStore(t)
	ctx := context.Background()
	if _, err := st.CreateRunner(ctx, store.Runner{
		Name: "bad-caps", Mode: store.RunnerModeCached, Base: "x", MemoryMax: "2Q",
	}); err != nil {
		t.Fatalf("CreateRunner: %v", err)
	}
	if _, err := resolveRunnerCaps(ctx, st, "bad-caps"); err == nil {
		t.Fatal("resolveRunnerCaps accepted an unparseable memory_max")
	}
}

// capsFromRunner is the non-empty Caps every tier-driver refusal test below
// exercises against.
var capsFromRunner = executor.Caps{MemoryMaxBytes: 512 << 20}

// TestTierDriversRefuseCapsWhenNotSandboxed covers the fail-closed axis
// applyJobCaps itself cannot see: it only runs inside the sandboxed drivers,
// so a job with non-empty caps that fell back to the plain subprocess
// boundary (tier pinned disabled, or degraded) must be refused before a plain
// executor.NewAnsible/NewTofu/NewCLI driver is ever built — otherwise the job
// would run fully uncapped with no refusal at all, silently.
func TestTierDriversRefuseCapsWhenNotSandboxed(t *testing.T) {
	dir := t.TempDir()
	useDataDir(t, dir) // pins TierDisabled — no sandbox, deterministic

	if _, err := ansibleTierDriver(dir, executor.AnsibleOptions{}, capsFromRunner); err == nil {
		t.Error("ansibleTierDriver ran capped at the disabled tier instead of refusing")
	} else if !strings.Contains(err.Error(), "enforcing") {
		t.Errorf("ansibleTierDriver error %q does not name the required tier", err)
	}

	if _, err := tofuTierDriver(dir, executor.TofuOptions{}, capsFromRunner); err == nil {
		t.Error("tofuTierDriver ran capped at the disabled tier instead of refusing")
	} else if !strings.Contains(err.Error(), "enforcing") {
		t.Errorf("tofuTierDriver error %q does not name the required tier", err)
	}

	if _, err := cliTierDriver(dir, executor.CLIOptions{}, capsFromRunner); err == nil {
		t.Error("cliTierDriver ran capped at the disabled tier instead of refusing")
	} else if !strings.Contains(err.Error(), "enforcing") {
		t.Errorf("cliTierDriver error %q does not name the required tier", err)
	}
}

// TestTierDriversRunUncappedAtDisabledTierByDefault is the non-regression
// counterpart: a job that names no runner (Empty caps, the overwhelming
// common case) must keep working exactly as before at the disabled tier — the
// new refusal is scoped to an actual caps request, not a blanket tightening.
func TestTierDriversRunUncappedAtDisabledTierByDefault(t *testing.T) {
	dir := t.TempDir()
	useDataDir(t, dir)

	if _, err := ansibleTierDriver(dir, executor.AnsibleOptions{}, executor.Caps{}); err != nil {
		t.Errorf("ansibleTierDriver with no caps refused at the disabled tier: %v", err)
	}
	if _, err := tofuTierDriver(dir, executor.TofuOptions{}, executor.Caps{}); err != nil {
		t.Errorf("tofuTierDriver with no caps refused at the disabled tier: %v", err)
	}
	if _, err := cliTierDriver(dir, executor.CLIOptions{}, executor.Caps{}); err != nil {
		t.Errorf("cliTierDriver with no caps refused at the disabled tier: %v", err)
	}
}

// resetCapsDecision clears the once-per-process caps-controller decision so a
// test can point it at a faked cgroup tree instead of the real host one, the
// same seam resetSandboxDecision gives the tier probe. root is forwarded to
// executor.PrepareCgroupController; "" would touch the real /sys/fs/cgroup,
// which no test here does.
func resetCapsDecision(t *testing.T, root string) {
	t.Helper()
	undecide := func() {
		jobCaps.once = sync.Once{}
		jobCaps.root = root
		jobCaps.ctrl = nil
		jobCaps.err = nil
	}
	undecide()
	t.Cleanup(func() {
		jobCaps.once = sync.Once{}
		jobCaps.root = ""
		jobCaps.ctrl = nil
		jobCaps.err = nil
	})
}

// fakeCgroupRoot builds a throwaway directory tree shaped like this
// process's real cgroup v2 mount, at the path selfCgroupDir(root) would
// resolve to, so PrepareCgroupController(root) exercises its real vacate +
// subtree_control logic without ever touching the real /sys/fs/cgroup. This
// mirrors internal/executor's own fakeCgroupTree helper (cgroup_internal_test.go);
// duplicated here because that one is unexported to its package.
func fakeCgroupRoot(t *testing.T, controllers string) string {
	t.Helper()
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skipf("no /proc/self/cgroup on this host: %v", err)
	}
	var rel string
	for _, line := range strings.Split(string(self), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			rel = p
			break
		}
	}
	if rel == "" {
		t.Skip("no unified cgroup line for this process")
	}
	root := t.TempDir()
	base := filepath.Join(root, filepath.Clean("/"+rel))
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup.controllers"), []byte(controllers), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup.procs"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestWithCapsRefusesFailClosedWithoutDelegation is the load-bearing
// mutation-target test: a job whose caps are non-empty but whose host has no
// delegated cgroup subtree (the ordinary case — this environment's own
// packaged-container posture, per docs/design/byo-runner.md §6) must reach
// SandboxOptions with Caps SET and Cgroup NIL, which is exactly the state
// internal/executor.applyJobCaps refuses on (sandbox_drivers.go). If withCaps
// stopped setting sb.Caps, this test would still show Cgroup nil but Caps
// would wrongly read Empty() too — the second assertion below is what a
// revert of the wiring actually breaks.
func TestWithCapsRefusesFailClosedWithoutDelegation(t *testing.T) {
	root := fakeCgroupRoot(t, "io pids") // no memory/cpu controller delegated
	resetCapsDecision(t, root)

	sb := withCaps(executor.SandboxOptions{DataDir: t.TempDir()}, capsFromRunner, io.Discard)
	if sb.Caps.Empty() {
		t.Fatal("withCaps dropped the caps instead of carrying them onto SandboxOptions")
	}
	if sb.Cgroup != nil {
		t.Fatal("withCaps produced a controller despite no delegated subtree; " +
			"applyJobCaps would wrongly treat this job as capped")
	}
}

// TestWithCapsWiresARealDelegatedController is the success-path counterpart:
// when the host DOES delegate memory+cpu, withCaps must hand applyJobCaps a
// live controller, not leave it nil out of over-caution.
func TestWithCapsWiresARealDelegatedController(t *testing.T) {
	root := fakeCgroupRoot(t, "cpu io memory pids")
	resetCapsDecision(t, root)

	sb := withCaps(executor.SandboxOptions{DataDir: t.TempDir()}, capsFromRunner, io.Discard)
	if sb.Caps.Empty() {
		t.Fatal("withCaps dropped the caps instead of carrying them onto SandboxOptions")
	}
	if sb.Cgroup == nil {
		t.Fatal("withCaps left Cgroup nil despite a delegated memory+cpu subtree; " +
			"a capped job would be refused even though it could run capped")
	}
}

// TestWithCapsIsNoopForEmptyCaps proves the daemon's own cgroup is left
// entirely alone for the common, capless job: withCaps must not even look at
// capsController when there is nothing to cap, so a fleet that never uses
// --runner never has its cgroup layout touched.
func TestWithCapsIsNoopForEmptyCaps(t *testing.T) {
	resetCapsDecision(t, "/does/not/exist") // would fail loudly if ever consulted

	sb := withCaps(executor.SandboxOptions{DataDir: t.TempDir()}, executor.Caps{}, io.Discard)
	if sb.Cgroup != nil {
		t.Error("withCaps prepared a controller for empty caps")
	}
	if jobCaps.ctrl != nil || jobCaps.err != nil {
		t.Error("withCaps consulted capsController for empty caps")
	}
}
