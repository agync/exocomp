package deps

import (
	"context"
	"errors"
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
	dir := t.TempDir()
	path := filepath.Join(dir, "uv")
	script := `#!/bin/sh
echo "$@" >> "` + counter + `"
args="$@"
case "$args" in
  *" venv "*|*"venv "*)
    for a in "$@"; do target="$a"; done
    mkdir -p "$target/bin" || exit 1
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

// TestEnsureVenvIsHashGated is the core of the ADR 0011 promise: a first
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

// uvAnsibleCoreConflictNarrative is uv's unsatisfiable-resolution message
// captured verbatim from `uv pip install ansible-core==2.21.3 ansible-runner==2.4.3
// -r requirements.txt` run against a requirements.txt containing only
// `ansible==11.3.0` — the real shape this fix has to recognize, line-wrapped
// and indented exactly as uv prints it.
const uvAnsibleCoreConflictNarrative = `  × No solution found when resolving dependencies:
  ╰─▶ Because ansible==11.3.0 depends on ansible-core>=2.18.3,<2.19.dev0 and
      you require ansible-core==2.21.3, we can conclude that your requirements
      and ansible==11.3.0 are incompatible.
      And because you require ansible==11.3.0, we can conclude that your
      requirements are unsatisfiable.`

// uvUnrelatedResolutionNarrative is an ordinary uv unsatisfiable error with no
// ansible-core involvement at all — captured verbatim from `uv pip install -r
// requirements.txt` against `jmespath==999.999.999`. It must never trigger
// the ansible-core wrap.
const uvUnrelatedResolutionNarrative = `  × No solution found when resolving dependencies:
  ╰─▶ Because there is no version of jmespath==999.999.999 and you require
      jmespath==999.999.999, we can conclude that your requirements are
      unsatisfiable.`

// TestWrapAnsibleCoreConflictNamesFixAndKeepsOriginal is the unit-level gate:
// fed uv's real narrative for the ansible-meta-package-vs-pinned-ansible-core
// conflict, the wrapped message must name the meta-package, the version it
// declared, the pinned ansible-core version, and the fix — while the original
// uv narrative stays reachable (wrap, don't discard).
func TestWrapAnsibleCoreConflictNamesFixAndKeepsOriginal(t *testing.T) {
	original := errors.New("deps: uv pip: exit status 1: " + uvAnsibleCoreConflictNarrative)

	wrapped := wrapAnsibleCoreConflict(original, "2.21.3")
	if wrapped == original {
		t.Fatal("conflict narrative was not wrapped")
	}
	got := wrapped.Error()
	for _, want := range []string{
		"'ansible' meta-package",
		"ansible==11.3.0",
		"ansible-core==2.21.3",
		"ADR 0006",
		"declare only the extra libraries",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapped message missing %q:\n%s", want, got)
		}
	}
	if !errors.Is(wrapped, original) {
		t.Error("wrapped error no longer wraps the original uv error")
	}
}

// TestWrapAnsibleCoreConflictPassesThroughOtherErrors: a uv resolution
// failure unrelated to the ansible meta-package must reach the operator
// unchanged — the wrap must never mask or reword a genuine resolution
// failure.
func TestWrapAnsibleCoreConflictPassesThroughOtherErrors(t *testing.T) {
	original := errors.New("deps: uv pip: exit status 1: " + uvUnrelatedResolutionNarrative)

	got := wrapAnsibleCoreConflict(original, "2.21.3")
	if got != original {
		t.Errorf("unrelated uv resolution error was rewritten: %v", got)
	}
}

// fakeUVAnsibleConflict writes a uv stand-in that completes the venv-creation
// step normally (so provision reaches the install step) and then fails `pip
// install` with uv's real unsatisfiable-ansible-core narrative, so the gate
// exercises EnsureVenv's actual error path end to end rather than only the
// helper in isolation.
func fakeUVAnsibleConflict(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "uv")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\" venv \"*|*\"venv \"*)\n" +
		"    for a in \"$@\"; do target=\"$a\"; done\n" +
		"    mkdir -p \"$target/bin\" || exit 1\n" +
		"    : > \"$target/bin/python\"; chmod +x \"$target/bin/python\"\n" +
		"    : > \"$target/bin/ansible-playbook\"; chmod +x \"$target/bin/ansible-playbook\"\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n" +
		"case \"$*\" in\n" +
		"  *\"pip install\"*)\n" +
		"    cat >&2 <<'EOF'\n" +
		uvAnsibleCoreConflictNarrative + "\n" +
		"EOF\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake uv: %v", err)
	}
	return path
}

// TestEnsureVenvWrapsAnsibleMetaPackageConflict is the end-to-end gate: a
// requirements.txt declaring the ansible meta-package, resolved against a uv
// stand-in that reproduces uv's real unsatisfiable-ansible-core narrative,
// must surface a wrapped error naming the meta-package and the pinned
// ansible-core version rather than the raw resolver narrative alone — and
// must leave no half-built venv behind (fail-closed, ADR 0011).
func TestEnsureVenvWrapsAnsibleMetaPackageConflict(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	uv := fakeUVAnsibleConflict(t)

	m := manifestFor(t, project, "ansible==11.3.0\n")
	spec := VenvSpec{CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv, AnsibleCore: "2.21.3"}

	_, err := EnsureVenv(context.Background(), spec)
	if err == nil {
		t.Fatal("EnsureVenv succeeded against an ansible/ansible-core conflict")
	}
	got := err.Error()
	for _, want := range []string{"'ansible' meta-package", "ansible-core==2.21.3", "ADR 0006"} {
		if !strings.Contains(got, want) {
			t.Errorf("EnsureVenv error missing %q:\n%s", want, got)
		}
	}
	if _, statErr := os.Stat(filepath.Join(cache, "demo", m.Hash)); !os.IsNotExist(statErr) {
		t.Errorf("half-built venv left behind after the wrapped failure (stat err = %v)", statErr)
	}
}
