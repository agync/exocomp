package executor_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// canaryEnv turns this test binary into the sandbox canary. The probe needs an
// executable that attempts the forbidden reaches from inside the sandbox, and
// re-entering the test binary is what makes the probe the real one — the same
// code path `exocomp doctor` runs, against a real nono.
const canaryEnv = "EXOCOMP_TEST_CANARY"

func TestMain(m *testing.M) {
	if os.Getenv(canaryEnv) == "" {
		os.Exit(m.Run())
	}
	targets, err := executor.ParseCanaryTargets(os.Args[1:])
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(executor.CanaryBadInvoke)
	}
	os.Exit(executor.RunCanary(os.Stdout, os.Stderr, targets))
}

func requireNono(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(executor.NonoBinary); err != nil {
		t.Skipf("%s not installed: %v", executor.NonoBinary, err)
	}
}

// TestProbeAssertsTheSandbox is the real probe: nono is on PATH, the canary
// runs under it, and the assertions are made against live targets.
func TestProbeAssertsTheSandbox(t *testing.T) {
	requireNono(t)
	t.Setenv(canaryEnv, "1")

	res := executor.Probe(context.Background(), executor.TierOpportunistic, executor.ProbeOptions{
		Canary: os.Args[0],
	})
	if res.Degraded() {
		t.Fatalf("probe degraded to %s: %s", res.Effective, res.Reason)
	}
	if !res.Enforced {
		t.Error("probe reported the tier without asserting it")
	}
	if res.LandlockABI < 1 {
		t.Errorf("Landlock ABI = %d, want at least 1", res.LandlockABI)
	}
	if !versionAtLeastFloor(res.NonoVersion) {
		t.Errorf("nono version %q is below the %s floor", res.NonoVersion, executor.NonoMinVersion)
	}
	t.Logf("tier=%s nono=%s landlock=v%d pathname-socket-mediated=%v",
		res.Effective, res.NonoVersion, res.LandlockABI, res.PathnameSocketMediated)
}

// TestProbeRefusesTheTierWithoutALauncher: a missing or unusable nono is a loud
// degradation to the disabled tier, never a silent pass.
func TestProbeRefusesTheTierWithoutALauncher(t *testing.T) {
	t.Setenv(canaryEnv, "1")

	res := executor.Probe(context.Background(), executor.TierEnforcing, executor.ProbeOptions{
		Binary: "exocomp-no-such-launcher",
		Canary: os.Args[0],
	})
	if !res.Degraded() {
		t.Fatalf("probe accepted %s without a launcher", res.Effective)
	}
	if res.Reason == "" {
		t.Error("degradation carries no reason")
	}
	if res.Effective != executor.TierDisabled {
		t.Errorf("effective tier = %s, want %s", res.Effective, executor.TierDisabled)
	}
}

// TestCanaryTargetsAreLiveWithoutTheSandbox is the mutation check on the probe
// itself: the same canary, the same targets, no sandbox — and every reach
// succeeds. Without this, a probe that passed because the key was missing or
// the socket was dead would read exactly like a probe that passed because the
// kernel refused.
func TestCanaryTargetsAreLiveWithoutTheSandbox(t *testing.T) {
	t.Setenv(canaryEnv, "1")

	root := t.TempDir()
	targets := executor.CanaryTargets{
		KeyPath:        filepath.Join(root, "host.key"),
		SocketPath:     filepath.Join(root, "probe.sock"),
		AbstractSocket: "exocomp-test-abstract",
	}
	if err := os.WriteFile(targets.KeyPath, []byte("not-a-real-key\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	for _, addr := range []string{targets.SocketPath, "@" + targets.AbstractSocket} {
		listen(t, "unix", addr)
	}
	targets.TCPAddr = listen(t, "tcp", "127.0.0.1:0")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, os.Args[0],
		targets.KeyPath, targets.SocketPath, targets.AbstractSocket, targets.TCPAddr).CombinedOutput()
	if err == nil {
		t.Fatalf("unsandboxed canary reported denials it could not have seen:\n%s", out)
	}

	report := string(out)
	for _, want := range []string{
		"file=ALLOWED", "abstract-socket=ALLOWED", "pathname-socket=ALLOWED", "tcp=ALLOWED",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("unsandboxed canary did not report %s:\n%s", want, report)
		}
	}
}

// listen starts a listener the test closes, and returns its address.
func listen(t *testing.T, network, address string) string {
	t.Helper()
	l, err := net.Listen(network, address)
	if err != nil {
		t.Fatalf("listen %s %s: %v", network, address, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return l.Addr().String()
}

// versionAtLeastFloor is the floor check the probe already made, restated here
// so the test asserts the version it reports rather than trusting it.
func versionAtLeastFloor(have string) bool {
	parts := strings.Split(strings.SplitN(have, "-", 2)[0], ".")
	floor := strings.Split(executor.NonoMinVersion, ".")
	for i := range floor {
		if i >= len(parts) {
			return false
		}
		a, b := atoi(parts[i]), atoi(floor[i])
		if a != b {
			return a > b
		}
	}
	return true
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}
