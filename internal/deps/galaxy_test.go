package deps

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestDiscoverGalaxySetsRoot: discovery reports the project root it settled
// on, the namespace the per-source collections cache is keyed by (the
// PythonManifest.Root contract, mirrored for the galaxy plane).
func TestDiscoverGalaxySetsRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "requirements.yml"), "collections:\n  - community.general\n")
	g, err := DiscoverGalaxy(root)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}
	if !g.Found() {
		t.Fatalf("galaxy not found: %+v", g)
	}
	if g.Root != root {
		t.Errorf("Root = %q, want %q", g.Root, root)
	}
}

// TestDiscoverGalaxyRootWalksUp: discovered from a playbook deep under the
// repo, Root is the repository root the walk settles on, not the start point.
// The walk shares FindProjectRoot's markers (see manifest.go), so the tree
// carries a .git marker the way a real repo does — a bare marker-less
// directory legitimately resolves to the start itself.
func TestDiscoverGalaxyRootWalksUp(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "requirements.yml"), "collections:\n  - community.general\n")
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	deep := filepath.Join(root, "playbooks", "site")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	g, err := DiscoverGalaxy(deep)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}
	if g.Root != root {
		t.Errorf("Root = %q, want %q", g.Root, root)
	}
}

// TestDiscoverGalaxyOverrideWins: an .exocomp/requirements.yml override beats
// the repo-root default, matching the Python manifest precedence.
func TestDiscoverGalaxyOverrideWins(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "requirements.yml"), "collections:\n  - community.general\n")
	write(t, filepath.Join(root, ".exocomp", "requirements.yml"), "collections:\n  - ansible.posix\n")
	g, err := DiscoverGalaxy(root)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}
	if !g.Found() {
		t.Fatalf("galaxy not found: %+v", g)
	}
	if filepath.Base(filepath.Dir(g.Path)) != ".exocomp" {
		t.Errorf("override not chosen: %s", g.Path)
	}
	if g.Root != root {
		t.Errorf("Root = %q, want %q", g.Root, root)
	}
}

// TestDiscoverGalaxyLocalSources pins the local-artifact extraction the
// resolver sandbox grants read on: a type: dir/file entry or a bare existing
// path is reported absolute, while a galaxy-server, git, or url entry — which
// galaxy dials rather than reads from disk — is not.
func TestDiscoverGalaxyLocalSources(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(t.TempDir(), "unifi_network")
	write(t, filepath.Join(local, "galaxy.yml"), "namespace: abn\nname: unifi_network\n")
	archive := filepath.Join(t.TempDir(), "abn-unifi_network-0.1.0.tar.gz")
	write(t, archive, "not a real tarball\n")
	relative := filepath.Join(root, "vendor", "local_coll")
	write(t, filepath.Join(relative, "galaxy.yml"), "namespace: abn\nname: local_coll\n")

	body := "collections:\n" +
		"  - community.general\n" +
		"  - name: ansible.posix\n" +
		"    source: https://galaxy.ansible.com/api/\n" +
		"  - name: " + local + "\n" +
		"    type: dir\n" +
		"  - name: " + archive + "\n" +
		"    type: file\n" +
		"  - vendor/local_coll\n" +
		"  - https://example.invalid/api/\n" +
		"  - name: git@github.com:abn/thing.git\n" +
		"    type: git\n"
	write(t, filepath.Join(root, "requirements.yml"), body)

	g, err := DiscoverGalaxy(root)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}
	want := []string{local, archive, relative}
	if !slices.Equal(g.LocalSources, want) {
		t.Errorf("LocalSources = %v, want %v", g.LocalSources, want)
	}
}

// TestDiscoverGalaxyLocalSourcesMalformedIsEmpty: a requirements file that does
// not parse yields no local sources rather than failing discovery, so galaxy
// still runs and reports the parse failure with its own clearer message.
func TestDiscoverGalaxyLocalSourcesMalformedIsEmpty(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "requirements.yml"), "collections: [unterminated\n")
	g, err := DiscoverGalaxy(root)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}
	if len(g.LocalSources) != 0 {
		t.Errorf("malformed requirements yielded local sources %v, want none", g.LocalSources)
	}
}

// TestDiscoverGalaxyNoneStillSetsRoot: a project with no requirements file
// still reports the root discovery settled on, so callers keying on Root stay
// correct for a source that never declares collections.
func TestDiscoverGalaxyNoneStillSetsRoot(t *testing.T) {
	root := t.TempDir()
	g, err := DiscoverGalaxy(root)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}
	if g.Found() {
		t.Fatalf("empty project reported requirements: %+v", g)
	}
	if g.Root != root {
		t.Errorf("Root = %q, want %q", g.Root, root)
	}
}
