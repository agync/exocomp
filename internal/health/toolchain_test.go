package health

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

// foundProbe is a ToolProbe for a tool that resolved to version, so the
// manifest comparison runs without the host's real toolchain.
func foundProbe(tool, version string) ToolProbe {
	return ToolProbe{
		Tool:    Tool{Name: tool},
		Found:   true,
		Version: version,
		Text:    "found at /usr/bin/" + tool,
	}
}

func writeToolchainManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "MANIFEST.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// TestOptionalToolsEntryFlagsAPinDeviation: a resolved version other than the
// manifest pin is a warn that names the tool and both versions — the escape
// hatch is a supported state (Decision 0006), so it is never a fail.
func TestOptionalToolsEntryFlagsAPinDeviation(t *testing.T) {
	probes := []ToolProbe{foundProbe("restic", "0.19.1"), foundProbe("tofu", "1.11.5")}
	path := writeToolchainManifest(t, `{"restic":"0.19.0","tofu":"1.11.5"}`)
	t.Setenv(ManifestEnv, path)

	entry := optionalToolsEntry(probes, ProbeToolchain(probes))
	if entry.Status != wire.HealthWarn {
		t.Errorf("status = %q, want warn (a pin deviation is never fail)", entry.Status)
	}
	for _, want := range []string{"restic", "0.19.1", "0.19.0"} {
		if !strings.Contains(entry.Detail, want) {
			t.Errorf("detail %q does not name %q", entry.Detail, want)
		}
	}
	if strings.Contains(entry.Detail, "tofu") {
		t.Errorf("detail %q names a tool that matched its pin", entry.Detail)
	}
}

// TestOptionalToolsEntryPinMatchIsOK: every resolvable tool at its pin is ok.
func TestOptionalToolsEntryPinMatchIsOK(t *testing.T) {
	probes := []ToolProbe{foundProbe("restic", "0.19.1"), foundProbe("tofu", "1.11.5")}
	path := writeToolchainManifest(t, `{"restic":"0.19.1","tofu":"1.11.5"}`)
	t.Setenv(ManifestEnv, path)

	entry := optionalToolsEntry(probes, ProbeToolchain(probes))
	if entry.Status != wire.HealthOK {
		t.Errorf("status = %q, want ok when every pin matches: %s", entry.Status, entry.Detail)
	}
}

// TestOptionalToolsEntryNoManifestIsUnknown: no manifest env is a state to
// read, not a fault.
func TestOptionalToolsEntryNoManifestIsUnknown(t *testing.T) {
	probes := []ToolProbe{foundProbe("restic", "0.19.1")}
	t.Setenv(ManifestEnv, "")

	entry := optionalToolsEntry(probes, ProbeToolchain(probes))
	if entry.Status != wire.HealthUnknown {
		t.Errorf("status = %q, want unknown with no manifest: %s", entry.Status, entry.Detail)
	}
}

// TestOptionalToolsEntryKeepsMissingReporting: adding the comparison must not
// disturb the existing missing-tool warn.
func TestOptionalToolsEntryKeepsMissingReporting(t *testing.T) {
	probes := []ToolProbe{
		foundProbe("tofu", "1.11.5"),
		{Tool: Tool{Name: "restic"}, Found: false, Text: "MISSING"},
	}
	path := writeToolchainManifest(t, `{"tofu":"1.11.5"}`)
	t.Setenv(ManifestEnv, path)

	entry := optionalToolsEntry(probes, ProbeToolchain(probes))
	if entry.Status != wire.HealthWarn {
		t.Errorf("status = %q, want warn for a missing tool", entry.Status)
	}
	if !strings.Contains(entry.Detail, "missing: restic") {
		t.Errorf("detail %q no longer reports the missing tool", entry.Detail)
	}
}

// TestToolVersionToken pins the per-tool extraction rules against the real
// version-probe first lines, and proves an unknown tool extracts nothing.
func TestToolVersionToken(t *testing.T) {
	cases := []struct{ tool, line, want string }{
		{"ansible-playbook", "ansible-playbook [core 2.21.4]", "2.21.4"},
		{"tofu", "OpenTofu v1.11.5", "1.11.5"},
		{"restic", "restic 0.19.1 compiled with go1.26.4 on linux/amd64", "0.19.1"},
		{"age", "v1.3.1", "1.3.1"},
		{"sops", "sops 3.13.3 (latest)", "3.13.3"},
		{"uv", "uv 0.12.15 (x86_64-unknown-linux-gnu)", "0.12.15"},
		{"git", "git version 2.55.0", "2.55.0"},
		{"nono", "nono 0.74.0", ""},
		{"restic", "restic (devel)", ""},
	}
	for _, c := range cases {
		got, _ := toolVersionToken(c.tool, c.line)
		if got != c.want {
			t.Errorf("toolVersionToken(%q, %q) = %q, want %q", c.tool, c.line, got, c.want)
		}
	}
}
