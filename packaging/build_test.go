package packaging

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// imageTestEnv opts into the real build. It is not part of `make check`: a
// podman build pulls base images and downloads every pinned tool, which is
// minutes against a network, and `make check` gates every commit. The gate for
// this milestone runs it explicitly, as does CI.
const imageTestEnv = "EXOCOMP_TEST_IMAGE"

// imageTag is this test's own tag, so a run never replaces the image an
// operator or `make image` built.
const imageTag = "localhost/exocomp:packaging-test"

// imageBuildTimeout covers a cold build: base images, the uv toolchain, and
// five pinned downloads.
const imageBuildTimeout = 30 * time.Minute

func requireImageBuild(t *testing.T) {
	t.Helper()
	if os.Getenv(imageTestEnv) == "" {
		t.Skipf("set %s=1 to build the image in this test", imageTestEnv)
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skipf("podman not installed: %v", err)
	}
}

// TestImageBuildsWithTheToolchainBakedIn is the packaging gate: the
// Containerfile builds, and the image that comes out resolves its own pinned
// toolchain rather than whatever the host happened to have. `doctor` is the
// probe because it is the same report an operator reads.
func TestImageBuildsWithTheToolchainBakedIn(t *testing.T) {
	requireImageBuild(t)

	out, err := runWithin(t, imageBuildTimeout,
		"podman", "build", "-t", imageTag, "-f", "Containerfile", "..")
	if err != nil {
		t.Fatalf("podman build: %v\n%s", err, tail(out))
	}
	t.Cleanup(func() {
		_ = exec.Command("podman", "rmi", "-f", imageTag).Run()
	})

	report, err := runWithin(t, 2*time.Minute, "podman", "run", "--rm", imageTag, "doctor")
	if err != nil {
		t.Fatalf("in-image doctor: %v\n%s", err, report)
	}

	// The toolchain is baked in if doctor resolves each tool inside the image.
	for _, want := range []string{
		"ansible-playbook   found at /opt/exocomp/bin/ansible-playbook",
		"tofu               found at /usr/local/bin/tofu",
		"restic             found at /usr/local/bin/restic",
		"age                found at /usr/local/bin/age",
		"sops               found at /usr/local/bin/sops",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("in-image doctor does not report %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, "core 2.21.3") {
		t.Errorf("the image resolved an ansible-core other than the pinned one:\n%s", report)
	}
	if !strings.Contains(report, "data dir:     /var/lib/exocomp — mode 0700") {
		t.Errorf("the image's data dir is not 0700:\n%s", report)
	}
	// The sandbox tier is the image's default, so the image ships nono and the
	// probe has to pass inside a rootless container — unprivileged Landlock
	// self-restriction is exactly what Decision 0005 chose nono for.
	if !strings.Contains(report, "tier               local-sandboxed") {
		t.Errorf("the image does not run jobs sandboxed:\n%s", report)
	}

	// A ping check needs the image's ping to work as its uid with no
	// capability, once the namespace's ping_group_range covers that gid.
	out, err = runWithin(t, time.Minute, "podman", "run", "--rm", "--network", "none",
		"--sysctl", "net.ipv4.ping_group_range=1000 1000",
		"--entrypoint", "ping", imageTag, "-c", "1", "-W", "2", "127.0.0.1")
	if err != nil {
		t.Errorf("unprivileged ping in the image: %v\n%s", err, out)
	}
}

// runWithin runs one command under a deadline and returns its combined output.
func runWithin(t *testing.T, limit time.Duration, name string, args ...string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()

	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("%s did not finish within %s", name, limit)
	}
	return string(out), err
}

// tail keeps the end of a long build log, which is where the failure is.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}
