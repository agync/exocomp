package deps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGalaxy writes an ansible-galaxy stand-in that records every invocation's
// argv to counter (one line per invocation) and its HOME/TMPDIR/PWD to
// envfile, and lays down a fake installed collection under the -p target so a
// provisioned dir is distinguishable from an empty one. It lets the
// hash-gating and env-redirection be tested deterministically and offline:
// what matters is whether ansible-galaxy is invoked at all and under what
// environment, not what it resolves.
func fakeGalaxy(t *testing.T, counter, envfile string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ansible-galaxy")
	script := `#!/bin/sh
echo "$@" >> "` + counter + `"
{ echo "HOME=$HOME"; echo "ANSIBLE_HOME=$ANSIBLE_HOME"; echo "TMPDIR=$TMPDIR"; echo "PWD=$PWD"; } > "` + envfile + `"
for a in "$@"; do target="$a"; done
mkdir -p "$target/ansible_collections/community/general" || exit 1
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ansible-galaxy: %v", err)
	}
	return path
}

// galaxyFor lays down a requirements.yml and discovers it, mirroring
// manifestFor for the galaxy plane.
func galaxyFor(t *testing.T, root, body string) GalaxyManifest {
	t.Helper()
	write(t, filepath.Join(root, "requirements.yml"), body)
	g, err := DiscoverGalaxy(root)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}
	if !g.Found() {
		t.Fatalf("galaxy has no requirements: %+v", g)
	}
	return g
}

// lastArgvLine is the most recent invocation the fake galaxy recorded.
func lastArgvLine(t *testing.T, counter string) string {
	t.Helper()
	body, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	return lines[len(lines)-1]
}

// seedCollections lays down a <hash> collections dir with a galaxy ready
// marker whose mtime is `at`, matching how a real provision leaves recency —
// the collections half of seedVenv.
func seedCollections(t *testing.T, sourceDir, hash string, at time.Time) string {
	t.Helper()
	dir := filepath.Join(sourceDir, hash)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir collections: %v", err)
	}
	marker := filepath.Join(dir, galaxyReadyMarker)
	if err := os.WriteFile(marker, []byte(hash+"\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.Chtimes(marker, at, at); err != nil {
		t.Fatalf("chtimes marker: %v", err)
	}
	return dir
}

// recordingSandbox records the argv it wrapped and passes it through
// unchanged, proving EnsureCollections routes the galaxy invocation through
// the spec's launcher without needing a real nono.
type recordingSandbox struct{ wrapped []string }

func (r *recordingSandbox) Wrap(argv []string) ([]string, error) {
	r.wrapped = append(r.wrapped, argv...)
	return argv, nil
}

// TestEnsureCollectionsIsHashGated is the core of the Decision 0015 promise: a
// first resolve invokes ansible-galaxy and provisions the dir; a second
// resolve of the same requirements invokes it zero more times and returns the
// same directory; changed requirements land a new hash directory and invoke
// ansible-galaxy again.
func TestEnsureCollectionsIsHashGated(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "galaxy-invocations")
	envfile := filepath.Join(t.TempDir(), "galaxy-env")
	galaxy := fakeGalaxy(t, counter, envfile)

	g := galaxyFor(t, project, "collections:\n  - community.general\n")
	spec := CollectionsSpec{CacheRoot: cache, Source: "demo", Manifest: g, GalaxyBinary: galaxy}

	first, err := EnsureCollections(context.Background(), spec)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if !first.Created {
		t.Fatal("first ensure did not report Created")
	}
	if first.Requirements != g.Path {
		t.Errorf("Requirements = %q, want %q", first.Requirements, g.Path)
	}
	if got := countLines(t, counter); got == 0 {
		t.Fatal("first ensure invoked ansible-galaxy zero times")
	}
	if _, err := os.Stat(filepath.Join(first.Dir, galaxyReadyMarker)); err != nil {
		t.Fatalf("ready marker absent after provision: %v", err)
	}

	// Unchanged requirements: the gate must reuse, invoking galaxy no further.
	second, err := EnsureCollections(context.Background(), spec)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if second.Created {
		t.Error("second ensure re-invoked ansible-galaxy for an unchanged manifest")
	}
	if second.Dir != first.Dir {
		t.Errorf("second dir %s != first %s", second.Dir, first.Dir)
	}
	if got := countLines(t, counter); got != 1 {
		t.Errorf("ansible-galaxy invoked %d times on cache hit, want 1", got)
	}

	// Changed requirements: a new hash directory, galaxy invoked again.
	g2 := galaxyFor(t, project, "collections:\n  - community.general\n  - community.docker\n")
	spec2 := spec
	spec2.Manifest = g2
	third, err := EnsureCollections(context.Background(), spec2)
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

// TestEnsureCollectionsFailClosed: an ansible-galaxy that exits non-zero must
// leave no reusable dir behind, so a run never proceeds against half-met
// collections (Decision 0015).
func TestEnsureCollectionsFailClosed(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	failing := filepath.Join(t.TempDir(), "ansible-galaxy")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil {
		t.Fatalf("write failing ansible-galaxy: %v", err)
	}

	g := galaxyFor(t, project, "collections:\n  - community.general\n")
	_, err := EnsureCollections(context.Background(), CollectionsSpec{
		CacheRoot: cache, Source: "demo", Manifest: g, GalaxyBinary: failing,
	})
	if err == nil {
		t.Fatal("EnsureCollections succeeded against a failing ansible-galaxy")
	}
	if _, statErr := os.Stat(filepath.Join(cache, "demo", g.Hash)); !os.IsNotExist(statErr) {
		t.Errorf("half-built collections dir left behind (stat err = %v)", statErr)
	}
}

// TestEnsureCollectionsOfflineFlag: an offline resolve passes --offline to
// ansible-galaxy so it serves from the cache alone with no network.
func TestEnsureCollectionsOfflineFlag(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "galaxy-invocations")
	envfile := filepath.Join(t.TempDir(), "galaxy-env")
	galaxy := fakeGalaxy(t, counter, envfile)

	g := galaxyFor(t, project, "collections:\n  - community.general\n")
	spec := CollectionsSpec{
		CacheRoot: cache, Source: "demo", Manifest: g, GalaxyBinary: galaxy, Offline: true,
	}
	if _, err := EnsureCollections(context.Background(), spec); err != nil {
		t.Fatalf("EnsureCollections: %v", err)
	}
	if line := lastArgvLine(t, counter); !strings.Contains(line, "--offline") {
		t.Errorf("offline resolve did not pass --offline: %s", line)
	}
}

// TestEnsureCollectionsRedirectsHomeAndTmpdir: the granted galaxy cache dirs
// are pre-created before ansible-galaxy runs (a Landlock write grant attaches
// only to an existing path), galaxy's HOME is redirected onto the cache root's
// ungranted home directory, and its ANSIBLE_HOME and TMPDIR are redirected
// onto the granted children — so its download/temp cache lands where the
// sandbox grants instead of the daemon's home, while the HOME nono keeps its
// state under stays outside every grant.
func TestEnsureCollectionsRedirectsHomeAndTmpdir(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	galaxyCache := filepath.Join(cache, "galaxy-cache")
	counter := filepath.Join(t.TempDir(), "galaxy-invocations")
	envfile := filepath.Join(t.TempDir(), "galaxy-env")
	galaxy := fakeGalaxy(t, counter, envfile)

	g := galaxyFor(t, project, "collections:\n  - community.general\n")
	spec := CollectionsSpec{
		CacheRoot: cache, Source: "demo", Manifest: g, GalaxyBinary: galaxy,
		GalaxyCacheDir: galaxyCache,
	}
	if _, err := EnsureCollections(context.Background(), spec); err != nil {
		t.Fatalf("EnsureCollections: %v", err)
	}
	for _, d := range []string{
		GalaxyHome(galaxyCache),
		GalaxyAnsibleHome(galaxyCache),
		GalaxyTmpDir(galaxyCache),
	} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("galaxy cache dir %s not pre-created before ansible-galaxy ran: %v", d, err)
		}
	}
	body, err := os.ReadFile(envfile)
	if err != nil {
		t.Fatalf("read galaxy env: %v", err)
	}
	env := string(body)
	if !strings.Contains(env, "HOME="+GalaxyHome(galaxyCache)) {
		t.Errorf("HOME not redirected onto the ungranted galaxy home:\n%s", env)
	}
	if !strings.Contains(env, "ANSIBLE_HOME="+GalaxyAnsibleHome(galaxyCache)) {
		t.Errorf("ANSIBLE_HOME not redirected onto the granted galaxy home child:\n%s", env)
	}
	if !strings.Contains(env, "TMPDIR="+GalaxyTmpDir(galaxyCache)) {
		t.Errorf("TMPDIR not redirected onto the granted galaxy tmp:\n%s", env)
	}
}

// TestGalaxyCacheGrantsNeverCoverResolverHome is the guard behind the galaxy
// cache layout: the resolver's HOME is deliberately never granted, because
// nono keeps its protected state roots under $HOME and refuses to launch when
// a grant would let the sandboxed process reach them. Every grant a galaxy
// cache root produces must stay clear of both state-root shapes nono
// protects.
func TestGalaxyCacheGrantsNeverCoverResolverHome(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache", "galaxy")
	home := GalaxyHome(root)
	for _, stateRoot := range []string{
		filepath.Join(home, ".local", "state", "nono"),
		filepath.Join(home, ".nono"),
	} {
		for _, grant := range GalaxyCacheGrants(root) {
			if coversDir(grant, stateRoot) {
				t.Errorf("grant %q covers nono state root %q", grant, stateRoot)
			}
		}
	}
	// The strongest form: no grant is the HOME itself or an ancestor of it.
	for _, grant := range GalaxyCacheGrants(root) {
		if coversDir(grant, home) {
			t.Errorf("grant %q covers the resolver HOME %q", grant, home)
		}
	}
}

// coversDir reports whether a grant on dir reaches path recursively — the
// semantics nono gives every path grant, and executor.covers (package-private
// there) — so the guard test asserts the same relationship the launcher would
// refuse.
func coversDir(dir, path string) bool {
	dir, path = filepath.Clean(dir), filepath.Clean(path)
	if dir == path {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// TestEnsureCollectionsPrunesStaleSiblings: a successful provision self-bounds
// the source's cache, removing stale siblings and protecting the dir it just
// built — the collections half of TestEnsureVenvPrunesStaleSiblingsAfterProvision.
func TestEnsureCollectionsPrunesStaleSiblings(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "galaxy-invocations")
	envfile := filepath.Join(t.TempDir(), "galaxy-env")
	galaxy := fakeGalaxy(t, counter, envfile)

	sourceDir := filepath.Join(cache, "demo")
	now := time.Now()
	staleNewer := seedCollections(t, sourceDir, "stale-newer", now.Add(-40*24*time.Hour))
	staleOlder := seedCollections(t, sourceDir, "stale-older", now.Add(-90*24*time.Hour))

	g := galaxyFor(t, project, "collections:\n  - community.general\n")
	spec := CollectionsSpec{
		CacheRoot: cache, Source: "demo", Manifest: g, GalaxyBinary: galaxy,
		KeepN: 1, KeepSince: 24 * time.Hour,
	}
	c, err := EnsureCollections(context.Background(), spec)
	if err != nil {
		t.Fatalf("EnsureCollections: %v", err)
	}
	if !c.Created {
		t.Fatal("expected a fresh provision")
	}
	mustNotExist(t, staleOlder)
	mustExist(t, staleNewer)
	mustExist(t, c.Dir)
}

// TestEnsureCollectionsWrapsArgvInSandbox: when the spec carries a sandbox,
// the ansible-galaxy argv is routed through the launcher — the resolver runs
// confined, not in the daemon context.
func TestEnsureCollectionsWrapsArgvInSandbox(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "galaxy-invocations")
	envfile := filepath.Join(t.TempDir(), "galaxy-env")
	galaxy := fakeGalaxy(t, counter, envfile)

	g := galaxyFor(t, project, "collections:\n  - community.general\n")
	sb := &recordingSandbox{}
	spec := CollectionsSpec{
		CacheRoot: cache, Source: "demo", Manifest: g, GalaxyBinary: galaxy, Sandbox: sb,
	}
	if _, err := EnsureCollections(context.Background(), spec); err != nil {
		t.Fatalf("EnsureCollections: %v", err)
	}
	joined := strings.Join(sb.wrapped, " ")
	if !strings.Contains(joined, galaxy) || !strings.Contains(joined, "collection install") {
		t.Errorf("sandbox did not wrap the ansible-galaxy argv: %v", sb.wrapped)
	}
	if !strings.Contains(joined, "-p "+filepath.Join(cache, "demo", g.Hash)) {
		t.Errorf("wrapped argv missing the -p target: %v", sb.wrapped)
	}
}

func TestEnsureCollectionsRejectsNoRequirements(t *testing.T) {
	_, err := EnsureCollections(context.Background(), CollectionsSpec{
		CacheRoot: t.TempDir(), Source: "demo",
	})
	if err == nil {
		t.Fatal("EnsureCollections accepted a source with no galaxy manifest")
	}
}

func TestEnsureCollectionsRejectsUnsafeSource(t *testing.T) {
	g := galaxyFor(t, t.TempDir(), "collections:\n  - community.general\n")
	_, err := EnsureCollections(context.Background(), CollectionsSpec{
		CacheRoot: t.TempDir(), Source: "../escape", Manifest: g,
	})
	if err == nil {
		t.Fatal("EnsureCollections accepted an unsafe source component")
	}
}

// TestRemoveProjectCaches: GC removes both dependency cache planes for a
// project's SourceKey — cache/python/<key> and cache/collections/<key> — and
// tolerates a missing key (a source that never synced, or already removed).
func TestRemoveProjectCaches(t *testing.T) {
	dataDir := t.TempDir()
	projectRoot := t.TempDir()
	key := SourceKey(projectRoot)
	pythonKey := filepath.Join(dataDir, "cache", "python", key)
	collectionsKey := filepath.Join(dataDir, "cache", "collections", key)
	for _, d := range []string{filepath.Join(pythonKey, "somehash"), filepath.Join(collectionsKey, "somehash")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	if err := RemoveProjectCaches(dataDir, projectRoot); err != nil {
		t.Fatalf("RemoveProjectCaches: %v", err)
	}
	for _, d := range []string{pythonKey, collectionsKey} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("plane %s not removed (stat err = %v)", d, err)
		}
	}

	// A second call with nothing left, and a key that never existed, are both
	// not errors.
	if err := RemoveProjectCaches(dataDir, projectRoot); err != nil {
		t.Fatalf("second RemoveProjectCaches: %v", err)
	}
	if err := RemoveProjectCaches(dataDir, filepath.Join(t.TempDir(), "never-seen")); err != nil {
		t.Fatalf("RemoveProjectCaches for a missing key: %v", err)
	}
}
