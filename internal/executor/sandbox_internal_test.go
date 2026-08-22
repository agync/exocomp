package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o700) }

func writeExecutable(t *testing.T, path, head string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(head), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		have, floor string
		want        bool
	}{
		{"0.74.0", "0.55.0", true},
		{"0.55.0", "0.55.0", true},
		{"0.54.9", "0.55.0", false},
		{"1.0", "0.55.0", true},
		{"0.55", "0.55.0", true},
		{"0.55.0-rc1", "0.55.0", true},
		{"", "0.55.0", false},
		{"nightly", "0.55.0", false},
	} {
		if got := versionAtLeast(tc.have, tc.floor); got != tc.want {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tc.have, tc.floor, got, tc.want)
		}
	}
}

// TestCoversIsRecursive: every nono path grant covers the whole subtree, so a
// guard that only compared paths for equality would pass a grant one directory
// above the host key.
func TestCoversIsRecursive(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		dir, path string
		want      bool
	}{
		{"/data", "/data/host.key", true},
		{"/data", "/data", true},
		{"/data", "/data/run/job-1", true},
		{"/", "/data/host.key", true},
		{"/data/run", "/data/host.key", false},
		{"/database", "/data/host.key", false},
		{"/data/", "/data/host.key", true},
	} {
		if got := covers(tc.dir, tc.path); got != tc.want {
			t.Errorf("covers(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
		}
	}
}

func TestBackendPort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		url  string
		want int
	}{
		{"http://127.0.0.1:41235/state/edge", 41235},
		{"https://example.invalid/state/edge", HTTPSPort},
		{"http://127.0.0.1/state/edge", 0},
		{"", 0},
		{"::not a url", 0},
	} {
		if got := backendPort(tc.url); got != tc.want {
			t.Errorf("backendPort(%q) = %d, want %d", tc.url, got, tc.want)
		}
	}
}

// TestToolDirsWidensOnlyForScripts: an interpreted tool needs the toolchain
// root beside its bin directory, and a compiled one does not — widening for
// both would grant all of ~/.local for a binary dropped in ~/.local/bin.
func TestToolDirsWidensOnlyForScripts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := mkdirAll(bin); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	script := filepath.Join(bin, "shim")
	writeExecutable(t, script, "#!/usr/bin/env python3\n")
	if got, want := toolDirs(script), []string{bin, dir}; !equalPaths(got, want) {
		t.Errorf("toolDirs(script) = %v, want %v", got, want)
	}

	compiled := filepath.Join(bin, "tool")
	writeExecutable(t, compiled, "\x7fELF\x02\x01\x01\x00")
	if got, want := toolDirs(compiled), []string{bin}; !equalPaths(got, want) {
		t.Errorf("toolDirs(compiled) = %v, want %v", got, want)
	}
}

func equalPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
