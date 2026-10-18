package deps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeUV writes a uv stand-in that records every invocation to counter and,
// for the `venv` subcommand, lays down the bin/python and bin/ansible-playbook
// a real venv would carry. It lets the hash-gating be tested deterministically
// and offline: what matters is whether uv is invoked at all, not what it
// resolves.
func fakeUV(t *testing.T, counter string) string {
	t.Helper()
	return fakeUVDelayed(t, counter, "0")
}

// fakeUVDelayed is fakeUV with a sleep (seconds, as sleep(1) takes it) inside
// the `venv` step, so concurrent callers overlap and a race is observable.
func fakeUVDelayed(t *testing.T, counter, delay string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "uv")
	script := `#!/bin/sh
echo "$@" >> "` + counter + `"
args="$@"
case "$args" in
  *" venv "*|*"venv "*)
    sleep ` + delay + `
    for a in "$@"; do target="$a"; done
    mkdir -p "$target/bin" || exit 1
    mkdir -p "$target/lib/python3.14/site-packages" || exit 1
    : > "$target/bin/python"; chmod +x "$target/bin/python"
    : > "$target/bin/ansible-playbook"; chmod +x "$target/bin/ansible-playbook"
    ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake uv: %v", err)
	}
	return path
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read counter: %v", err)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

func manifestFor(t *testing.T, root, body string) PythonManifest {
	t.Helper()
	write(t, filepath.Join(root, "requirements.txt"), body)
	m, err := DiscoverPython(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !m.HasDeps() {
		t.Fatalf("manifest has no deps: %+v", m)
	}
	return m
}

// TestEnsureVenvIsHashGated is the core of the Decision 0011 promise: a first
// resolve invokes uv and provisions the venv; a second resolve of the same
// manifest invokes uv zero more times and returns the same directory; a changed
// manifest lands a new hash directory and invokes uv again.
func TestEnsureVenvIsHashGated(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "uv-invocations")
	uv := fakeUV(t, counter)

	m := manifestFor(t, project, "jmespath==1.1.0\n")
	spec := VenvSpec{CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv}

	first, err := EnsureVenv(context.Background(), spec)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if !first.Created {
		t.Fatal("first ensure did not report Created")
	}
	afterFirst := countLines(t, counter)
	if afterFirst == 0 {
		t.Fatal("first ensure invoked uv zero times")
	}
	if _, err := os.Stat(first.marker()); err != nil {
		t.Fatalf("ready marker absent after provision: %v", err)
	}

	// Unchanged manifest: the gate must reuse, invoking uv no further.
	second, err := EnsureVenv(context.Background(), spec)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if second.Created {
		t.Error("second ensure re-invoked uv for an unchanged manifest")
	}
	if second.Dir != first.Dir {
		t.Errorf("second dir %s != first %s", second.Dir, first.Dir)
	}
	if got := countLines(t, counter); got != afterFirst {
		t.Errorf("uv invoked %d more times on cache hit (was %d, now %d)", got-afterFirst, afterFirst, got)
	}

	// Changed manifest: a new hash directory, uv invoked again.
	m2 := manifestFor(t, project, "jmespath==1.1.0\nnetaddr\n")
	spec2 := spec
	spec2.Manifest = m2
	third, err := EnsureVenv(context.Background(), spec2)
	if err != nil {
		t.Fatalf("third ensure: %v", err)
	}
	if !third.Created {
		t.Error("changed manifest did not re-provision")
	}
	if third.Dir == first.Dir {
		t.Errorf("changed manifest reused the old hash dir %s", third.Dir)
	}
	if filepath.Base(third.Dir) == filepath.Base(first.Dir) {
		t.Error("changed manifest produced the same hash directory name")
	}
}

// TestEnsureVenvFailClosed: a uv that exits non-zero must leave no reusable
// venv behind, so a run never proceeds against half-met dependencies.
func TestEnsureVenvFailClosed(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	dir := t.TempDir()
	failing := filepath.Join(dir, "uv")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatalf("write failing uv: %v", err)
	}

	m := manifestFor(t, project, "jmespath\n")
	_, err := EnsureVenv(context.Background(), VenvSpec{
		CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: failing,
	})
	if err == nil {
		t.Fatal("EnsureVenv succeeded against a failing uv")
	}
	if _, statErr := os.Stat(filepath.Join(cache, "demo", m.Hash)); !os.IsNotExist(statErr) {
		t.Errorf("half-built venv left behind (stat err = %v)", statErr)
	}
}

func TestEnsureVenvRejectsNoDeps(t *testing.T) {
	_, err := EnsureVenv(context.Background(), VenvSpec{
		CacheRoot: t.TempDir(), Source: "demo", Manifest: PythonManifest{Kind: KindNone},
	})
	if err == nil {
		t.Fatal("EnsureVenv accepted a no-deps manifest")
	}
}

func TestEnsureVenvRejectsUnsafeSource(t *testing.T) {
	m := manifestFor(t, t.TempDir(), "jmespath\n")
	_, err := EnsureVenv(context.Background(), VenvSpec{
		CacheRoot: t.TempDir(), Source: "../escape", Manifest: m,
	})
	if err == nil {
		t.Fatal("EnsureVenv accepted an unsafe source component")
	}
}

// TestEnsureVenvInstallsOnlyDeclaredDeps is the Decision 0016 gate: a per-source venv
// carries only the project's declared libraries. ansible-core is a command, not
// a project dependency, so the captured `uv pip install` argv must name the
// manifest and must not force ansible-core or ansible-runner into the venv.
func TestEnsureVenvInstallsOnlyDeclaredDeps(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "uv-invocations")
	uv := fakeUV(t, counter)

	m := manifestFor(t, project, "jmespath==1.1.0\n")
	if _, err := EnsureVenv(context.Background(), VenvSpec{
		CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv,
	}); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	install := uvLineContaining(t, counter, "pip install")
	if !strings.Contains(install, m.Path) {
		t.Errorf("install argv %q does not name the manifest %q", install, m.Path)
	}
	for _, forced := range []string{"ansible-core==", "ansible-runner=="} {
		if strings.Contains(install, forced) {
			t.Errorf("install argv still forces %s into the project venv: %q", forced, install)
		}
	}
}

// TestEnsureVenvPinsInterpreterFromManifest: the venv is built with the
// interpreter the image froze into its manifest (Decision 0016), so its
// site-packages are ABI-compatible with the fallback ansible toolchain. No
// reachable manifest omits --python and leaves uv's own default.
func TestEnsureVenvPinsInterpreterFromManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MANIFEST.json")
	if err := os.WriteFile(path, []byte(`{"python":"3.14.0"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Setenv(manifestEnv, path)

	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "uv-invocations")
	uv := fakeUV(t, counter)
	m := manifestFor(t, project, "jmespath==1.1.0\n")
	if _, err := EnsureVenv(context.Background(), VenvSpec{
		CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv,
	}); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	if venv := uvLineContaining(t, counter, "venv"); !strings.Contains(venv, "--python 3.14.0") {
		t.Errorf("venv argv %q does not pin the manifest interpreter", venv)
	}
}

// TestEnsureVenvSitePackages: the venv's site-packages is the PYTHONPATH bridge
// target (Decision 0016), so it must sit under the venv root — the path the run
// sandbox already grants read for the venv.
func TestEnsureVenvSitePackages(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	uv := fakeUV(t, filepath.Join(t.TempDir(), "uv-invocations"))
	m := manifestFor(t, project, "jmespath==1.1.0\n")

	v, err := EnsureVenv(context.Background(), VenvSpec{
		CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv,
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	want := filepath.Join(v.Dir, "lib", "python3.14", "site-packages")
	if v.SitePackages != want {
		t.Errorf("SitePackages = %q, want %q", v.SitePackages, want)
	}
	if !strings.HasPrefix(v.SitePackages, v.Dir+string(os.PathSeparator)) {
		t.Errorf("SitePackages %q is not under the venv root %q", v.SitePackages, v.Dir)
	}
}

// uvLineContaining returns the recorded uv invocation containing marker, or
// fails when no invocation did. Each recorded line is one full argv.
func uvLineContaining(t *testing.T, counter, marker string) string {
	t.Helper()
	body, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read uv invocations: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	t.Fatalf("no uv invocation containing %q: %q", marker, body)
	return ""
}
