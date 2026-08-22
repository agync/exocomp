package packaging

import (
	"os"
	"strings"
	"testing"
)

// dataDirEnv is what cmd/exocomp resolves its data dir from. The value has to
// be the parent of the mount, because the binary appends "exocomp" to it.
const dataDirEnv = "XDG_DATA_HOME=/var/lib"

func readPackagingFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// TestContainerfileRunsTheDaemon: the entrypoint is the CLI, whose default
// subcommand is nothing. Without a CMD the container starts, prints the help,
// and exits — a control plane that never ran.
func TestContainerfileRunsTheDaemon(t *testing.T) {
	body := readPackagingFile(t, "Containerfile")

	if !strings.Contains(body, `CMD ["serve"]`) {
		t.Error("Containerfile names no default subcommand; the image would print help and exit")
	}
	if !strings.Contains(body, "ENV "+dataDirEnv) {
		t.Errorf("Containerfile does not point %s at the volume's parent", dataDirEnv)
	}
	if !strings.Contains(body, `VOLUME ["/var/lib/exocomp"]`) {
		t.Error("Containerfile no longer declares the data volume the env above assumes")
	}
}

// TestQuadletNamesTheSubcommand: the unit says what it runs rather than
// inheriting it, and points the daemon at the volume it mounts.
func TestQuadletNamesTheSubcommand(t *testing.T) {
	body := readPackagingFile(t, "quadlet/exocomp.container")

	if !strings.Contains(body, "\nExec=serve") {
		t.Error("the quadlet does not name the subcommand it runs")
	}
	if !strings.Contains(body, "Environment="+dataDirEnv) {
		t.Errorf("the quadlet does not set %s, so the daemon writes outside the volume", dataDirEnv)
	}
	if !strings.Contains(body, "Volume=exocomp.volume:/var/lib/exocomp") {
		t.Error("the quadlet no longer mounts the data volume the env above assumes")
	}
}

// TestContainerfileBakesTheToolchain: ADR 0006's determinism half. The image
// resolves its own pinned toolchain, and the operator escape hatch stays
// opt-in, so a restore comes back with the versions that produced the backup.
func TestContainerfileBakesTheToolchain(t *testing.T) {
	body := readPackagingFile(t, "Containerfile")

	for _, want := range []string{
		"uv python install",
		"uv tool install",
		"--constraints /tmp/constraints.txt",
		"ansible-runner",
		"UV_TOOL_DIR=/opt/exocomp/toolchain",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Containerfile does not %q; the Python layer is not the pinned one", want)
		}
	}
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "UV_TOOL_DIR=/var/lib") {
			t.Error("the image's default toolchain points at the data volume; " +
				"a fresh restore has the image before it has the volume (ADR 0006)")
		}
	}
	if !strings.Contains(body, "HOME=/home/exocomp") {
		t.Error("the image gives its uid no home; ansible-core refuses to start without one")
	}
}

// TestEveryFetchedToolIsPinnedByDigest: a tool downloaded at build time is
// supply chain, and a version tag is not a pin. Every artifact the build
// fetches has to be checked against a recorded sha256 for both architectures.
func TestEveryFetchedToolIsPinnedByDigest(t *testing.T) {
	body := readPackagingFile(t, "Containerfile")
	digests := readPackagingFile(t, "tool-digests.txt")

	if !strings.Contains(body, "sha256sum -c -") {
		t.Fatal("the build does not verify what it downloads")
	}
	for _, tool := range []string{"tofu", "restic", "age", "sops", "nono"} {
		if !strings.Contains(body, "fetch "+tool+" ") {
			t.Errorf("the build does not fetch %s through the checked path", tool)
		}
		for _, arch := range []string{"amd64", "arm64"} {
			if !strings.Contains(digests, tool+" "+arch+" ") {
				t.Errorf("no sha256 pinned for %s on %s", tool, arch)
			}
		}
	}
}

// TestConstraintsPinTheResolvedSet: `uv tool install` consumes no lockfile, so
// the committed constraints file is what keeps a transitive dependency from
// moving between two builds of the same tag (ADR 0006).
func TestConstraintsPinTheResolvedSet(t *testing.T) {
	body := readPackagingFile(t, "toolchain-constraints.txt")

	pinned := 0
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "==") {
			t.Errorf("constraints line %q is not an exact pin", line)
			continue
		}
		pinned++
	}
	if pinned < 10 {
		t.Errorf("only %d dependencies pinned; the resolved set is larger than that", pinned)
	}
	for _, want := range []string{"ansible-core==", "ansible-runner==", "cryptography==", "jinja2=="} {
		if !strings.Contains(body, want) {
			t.Errorf("constraints do not pin %s", strings.TrimSuffix(want, "=="))
		}
	}
}
