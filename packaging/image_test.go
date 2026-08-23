package packaging

import (
	"encoding/json"
	"os"
	"regexp"
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

// TestQuadletTmpfsBacksTheRunDir: the per-job run dir is a tmpfs child of the
// data volume, so a staged KUBECONFIG file (bootstrap-first Phase 3) lives in
// RAM. noswap is required — without it the RAM-only guarantee the file-staging
// assertion depends on does not hold.
func TestQuadletTmpfsBacksTheRunDir(t *testing.T) {
	body := readPackagingFile(t, "quadlet/exocomp.container")

	var line string
	for l := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(l, "Tmpfs=/var/lib/exocomp/run") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("the quadlet does not tmpfs-back the per-job run dir; a staged secret would land on the durable volume")
	}
	for _, opt := range []string{"noswap", "mode=0700"} {
		if !strings.Contains(line, opt) {
			t.Errorf("the run tmpfs mount %q lacks %q", line, opt)
		}
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

// containerfileArg returns the value of a `ARG NAME=value` line in the
// Containerfile, so a test can assert the committed manifest tracks the build's
// own pins.
func containerfileArg(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^ARG ` + regexp.QuoteMeta(name) + `=(\S+)`).FindStringSubmatch(body)
	if len(m) < 2 {
		t.Fatalf("Containerfile has no ARG %s", name)
	}
	return m[1]
}

// TestManifestPinsMatchTheContainerfile: MANIFEST.json is the explicit,
// committed source of the toolchain pins (ADR 0006), so it must not drift from
// the ARGs the build actually fetches and installs. git is the one field the
// committed file leaves empty — the build fills it from Fedora's resolved
// version — so it is asserted empty here rather than against an ARG.
func TestManifestPinsMatchTheContainerfile(t *testing.T) {
	var pins map[string]string
	if err := json.Unmarshal([]byte(readPackagingFile(t, "MANIFEST.json")), &pins); err != nil {
		t.Fatalf("MANIFEST.json is not valid JSON: %v", err)
	}
	dockerfile := readPackagingFile(t, "Containerfile")

	for field, arg := range map[string]string{
		"ansible_core":   "ANSIBLE_CORE_VERSION",
		"ansible_runner": "ANSIBLE_RUNNER_VERSION",
		"python":         "PYTHON_VERSION",
		"tofu":           "TOFU_VERSION",
		"restic":         "RESTIC_VERSION",
		"age":            "AGE_VERSION",
		"sops":           "SOPS_VERSION",
		"nono":           "NONO_VERSION",
	} {
		if want := containerfileArg(t, dockerfile, arg); pins[field] != want {
			t.Errorf("MANIFEST.json %q = %q, Containerfile %s = %q", field, pins[field], arg, want)
		}
	}

	// uv comes from a pinned base-image tag, not an ARG.
	uv := regexp.MustCompile(`ghcr\.io/astral-sh/uv:(\S+) AS uv`).FindStringSubmatch(dockerfile)
	if len(uv) < 2 {
		t.Fatal("Containerfile has no pinned uv base image")
	}
	if pins["uv"] != uv[1] {
		t.Errorf("MANIFEST.json uv = %q, Containerfile uv image = %q", pins["uv"], uv[1])
	}

	if pins["git"] != "" {
		t.Errorf("committed MANIFEST.json git = %q, want empty (the build resolves it)", pins["git"])
	}
}

// TestRuntimeReadsTheToolchainManifest: shipping the manifest is only half the
// fix — the daemon must actually read it, so a per-source venv's default
// ansible-core comes from the shipped pin rather than the compiled fallback.
// The runtime stage copies the committed file in and points the env var at it.
func TestRuntimeReadsTheToolchainManifest(t *testing.T) {
	body := readPackagingFile(t, "Containerfile")
	if !strings.Contains(body, "COPY packaging/MANIFEST.json /opt/exocomp/MANIFEST.json") {
		t.Error("the build does not copy the committed MANIFEST.json into the image")
	}
	if !strings.Contains(body, "EXOCOMP_TOOLCHAIN_MANIFEST=/opt/exocomp/MANIFEST.json") {
		t.Error("the image does not point EXOCOMP_TOOLCHAIN_MANIFEST at the shipped manifest; " +
			"the venv default would silently fall back to the compiled pin")
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
