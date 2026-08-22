package deps

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDiscoverPythonPrecedence(t *testing.T) {
	root := t.TempDir()
	// All three present: the .exocomp override must win.
	write(t, filepath.Join(root, "requirements.txt"), "netaddr\n")
	write(t, filepath.Join(root, "pyproject.toml"), "[project]\ndependencies = [\"boto3\"]\n")
	write(t, filepath.Join(root, ".exocomp", "requirements.txt"), "jmespath==1.1.0\n")

	m, err := DiscoverPython(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if m.Kind != KindRequirements {
		t.Fatalf("kind = %q, want requirements", m.Kind)
	}
	if filepath.Base(filepath.Dir(m.Path)) != ".exocomp" {
		t.Errorf("override not chosen: %s", m.Path)
	}
}

func TestDiscoverPythonPyprojectOverRequirements(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "requirements.txt"), "netaddr\n")
	write(t, filepath.Join(root, "pyproject.toml"), "[project]\ndependencies = [\"boto3\"]\n")

	m, err := DiscoverPython(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if m.Kind != KindPyproject {
		t.Fatalf("kind = %q, want pyproject", m.Kind)
	}
}

func TestDiscoverPythonPyprojectNoDepsFallsThrough(t *testing.T) {
	root := t.TempDir()
	// pyproject with only tool config declares no runtime deps, so a
	// requirements.txt beside it should still be the manifest.
	write(t, filepath.Join(root, "pyproject.toml"), "[tool.black]\nline-length = 100\n")
	write(t, filepath.Join(root, "requirements.txt"), "jmespath\n")

	m, err := DiscoverPython(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if m.Kind != KindRequirements {
		t.Fatalf("kind = %q, want requirements (pyproject had no deps)", m.Kind)
	}
}

func TestDiscoverPythonNone(t *testing.T) {
	m, err := DiscoverPython(t.TempDir())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if m.HasDeps() {
		t.Fatalf("empty project reported deps: %+v", m)
	}
}

func TestDiscoverPythonEmptyRequirementsIsNone(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "requirements.txt"), "# just a comment\n\n")
	m, err := DiscoverPython(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if m.HasDeps() {
		t.Fatalf("comment-only requirements reported deps: %+v", m)
	}
}

func TestManifestHashChangesWithContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "requirements.txt")

	write(t, path, "jmespath==1.1.0\n")
	first, _ := DiscoverPython(root)

	write(t, path, "jmespath==1.1.0\nnetaddr\n")
	second, _ := DiscoverPython(root)

	if first.Hash == "" || second.Hash == "" {
		t.Fatalf("empty hash: %q %q", first.Hash, second.Hash)
	}
	if first.Hash == second.Hash {
		t.Errorf("hash did not change with content: %q", first.Hash)
	}
}

func TestFindProjectRootWalksUp(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "requirements.txt"), "jmespath\n")
	deep := filepath.Join(root, "playbooks", "site")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := FindProjectRoot(deep); got != root {
		t.Errorf("FindProjectRoot(%s) = %s, want %s", deep, got, root)
	}
}

func TestDiscoverGalaxy(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "requirements.yml"), "collections:\n  - community.general\n")
	g, err := DiscoverGalaxy(root)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}
	if !g.Found() || g.Hash == "" {
		t.Fatalf("galaxy not found: %+v", g)
	}
}
