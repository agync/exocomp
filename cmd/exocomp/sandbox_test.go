package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// useDataDir points this process — and every child it starts — at a scratch
// data dir, and pins the disabled tier.
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
	t.Setenv(sandboxTierEnv, string(executor.TierDisabled))
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

// TestPinnedDisabledTierSkipsTheProbe: asking for the disabled tier is a
// decision, not a degradation, and it must not be reported as one.
func TestPinnedDisabledTierSkipsTheProbe(t *testing.T) {
	resetSandboxDecision(t)
	t.Setenv(sandboxTierEnv, string(executor.TierDisabled))

	res := sandboxTier(context.Background())
	if res.Effective != executor.TierDisabled {
		t.Errorf("effective tier = %s, want %s", res.Effective, executor.TierDisabled)
	}
	if res.Degraded() {
		t.Error("a pinned disabled tier is reported as a degradation")
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

// TestExplicitSandboxFailsClosed: a named enforcing request that the host
// cannot enforce is a refusal, not a silent downgrade — running the fleet's
// jobs unsandboxed is not what was asked for.
func TestExplicitSandboxFailsClosed(t *testing.T) {
	resetSandboxDecision(t)
	sandbox.requested = executor.TierEnforcing
	sandbox.result = executor.ProbeResult{
		Requested: executor.TierEnforcing,
		Effective: executor.TierDisabled,
		Reason:    "Landlock unavailable on this kernel",
	}
	sandbox.once.Do(func() {})

	if err := enforceSandboxTier(context.Background()); err == nil {
		t.Fatal("an unenforceable explicit enforcing request was allowed to run")
	}
}

// TestOpportunisticTierDegradesWithoutRefusing: opportunistic means "sandbox
// where the kernel allows it, and say so loudly where it does not", so a
// degraded opportunistic tier runs.
func TestOpportunisticTierDegradesWithoutRefusing(t *testing.T) {
	resetSandboxDecision(t)
	sandbox.requested = executor.TierOpportunistic
	sandbox.result = executor.ProbeResult{
		Requested: executor.TierOpportunistic,
		Effective: executor.TierDisabled,
		Reason:    "Landlock unavailable on this kernel",
	}
	sandbox.once.Do(func() {})

	if err := enforceSandboxTier(context.Background()); err != nil {
		t.Fatalf("opportunistic tier refused to degrade: %v", err)
	}
}

// TestSandboxOptionsCarryThePythonInstallDir: every kind starts from the grant
// set that names the tool trees, and the interpreter those trees point back
// into is part of it. A value inside the data dir is dropped rather than
// handed to every job.
func TestSandboxOptionsCarryThePythonInstallDir(t *testing.T) {
	python := t.TempDir()
	data := t.TempDir()
	resolved, err := filepath.EvalSymlinks(python)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	t.Setenv(executor.PythonInstallEnv, python)
	if got := sandboxOptions(data).ReadPaths; !slices.Contains(got, resolved) {
		t.Errorf("read grants %v lack the Python install dir %s", got, resolved)
	}

	inside := filepath.Join(data, "run")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv(executor.PythonInstallEnv, inside)
	insideResolved, err := filepath.EvalSymlinks(inside)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := sandboxOptions(data).ReadPaths; slices.Contains(got, inside) || slices.Contains(got, insideResolved) {
		t.Errorf("read grants %v reach inside the data dir via %s", got, inside)
	}
}

// TestResolverHostsAreDefaultsPlusExtras: an operator's extra hosts join a
// resolver's own, and neither list is changed by the other.
func TestResolverHostsAreDefaultsPlusExtras(t *testing.T) {
	old := resolverExtraHosts
	t.Cleanup(func() { resolverExtraHosts = old })

	own := []string{"pypi.org"}
	resolverExtraHosts = nil
	if got := resolverHosts(own); !slices.Equal(got, []string{"pypi.org"}) {
		t.Errorf("resolverHosts without extras = %v", got)
	}
	resolverExtraHosts = []string{"index.example.net"}
	if got, want := resolverHosts(own), []string{"pypi.org", "index.example.net"}; !slices.Equal(got, want) {
		t.Errorf("resolverHosts = %v, want %v", got, want)
	}
	if !slices.Equal(own, []string{"pypi.org"}) {
		t.Errorf("the resolver's own hosts were modified: %v", own)
	}
}

// TestNormalizeResolverHosts: the operator's extra hosts are lowercased and
// de-duplicated, and anything that is not a plain DNS name stops the daemon at
// startup instead of reaching the sandbox.
func TestNormalizeResolverHosts(t *testing.T) {
	got, err := normalizeResolverHosts([]string{"Index.Example.Net", "pypi.python.org", "index.example.net"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if want := []string{"index.example.net", "pypi.python.org"}; !slices.Equal(got, want) {
		t.Errorf("normalized hosts = %v, want %v", got, want)
	}
	for _, bad := range []string{"", "*", "*.example.com", "localhost", "127.0.0.1", "https://pypi.org", "pypi.org:443"} {
		if _, err := normalizeResolverHosts([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestResolversRefuseWithoutProxyMode: a launcher whose proxy mode failed the
// probe leaves the other job kinds sandboxed, and the resolvers fail closed
// without launching anything.
func TestResolversRefuseWithoutProxyMode(t *testing.T) {
	resetSandboxDecision(t)
	sandbox.requested = executor.TierEnforcing
	sandbox.result = executor.ProbeResult{
		Requested: executor.TierEnforcing, Effective: executor.TierEnforcing, Enforced: true,
		ProxyEnforced: false, ProxyReason: "proxy mode: canary exited 4 under the sandbox",
	}
	sandbox.once.Do(func() {})

	nonoLog := filepath.Join(t.TempDir(), "nono-argv")
	binDir := t.TempDir()
	fakeNono(t, binDir, nonoLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	project := t.TempDir()
	writeFile(t, filepath.Join(project, "requirements.txt"), "jmespath==1.1.0\n")
	writeFile(t, filepath.Join(project, "requirements.yml"), "collections:\n  - community.sops\n")
	play := filepath.Join(project, "site.yml")

	if _, err := resolveProjectVenv(context.Background(), t.TempDir(), play); err == nil || !strings.Contains(err.Error(), "proxy mode is not enforced") {
		t.Errorf("uv resolve error = %v, want a proxy mode refusal", err)
	}
	// A project with no Python dependencies reaches the collections resolver's own
	// check, since with them the uv resolver refuses first.
	galaxyOnly := t.TempDir()
	writeFile(t, filepath.Join(galaxyOnly, "requirements.yml"), "collections:\n  - community.sops\n")
	if _, err := resolveSourceCollections(context.Background(), t.TempDir(), filepath.Join(galaxyOnly, "site.yml")); err == nil || !strings.Contains(err.Error(), "proxy mode is not enforced") {
		t.Errorf("collections resolve error = %v, want a proxy mode refusal", err)
	}
	if _, err := os.Stat(nonoLog); err == nil {
		t.Errorf("a resolver launched under the sandbox anyway:\n%s", readFile(t, nonoLog))
	}
}

// TestConfigureResolverHosts: the serve flag sets the extras once, normalized,
// and a refused host leaves them as they were.
func TestConfigureResolverHosts(t *testing.T) {
	old := resolverExtraHosts
	t.Cleanup(func() { resolverExtraHosts = old })

	resolverExtraHosts = nil
	if err := configureResolverHosts([]string{"Index.Example.Net"}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if want := []string{"index.example.net"}; !slices.Equal(resolverExtraHosts, want) {
		t.Errorf("extras = %v, want %v", resolverExtraHosts, want)
	}
	if err := configureResolverHosts([]string{"ok.example.net", "*"}); err == nil {
		t.Error("a wildcard was accepted")
	}
	if want := []string{"index.example.net"}; !slices.Equal(resolverExtraHosts, want) {
		t.Errorf("a refused flag changed the extras to %v", resolverExtraHosts)
	}
}
