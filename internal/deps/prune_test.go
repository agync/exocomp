package deps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedVenv lays down a <hash> venv directory under sourceDir with a ready
// marker whose mtime is `at`, matching how a real provision leaves recency
// (see provision in venv.go): the marker is written once, at the end of a
// successful build, and never touched again.
func seedVenv(t *testing.T, sourceDir, hash string, at time.Time) string {
	t.Helper()
	dir := filepath.Join(sourceDir, hash)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir venv: %v", err)
	}
	marker := filepath.Join(dir, readyMarker)
	if err := os.WriteFile(marker, []byte(hash+"\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := os.Chtimes(marker, at, at); err != nil {
		t.Fatalf("chtimes marker: %v", err)
	}
	return dir
}

// seedVenvNoMarker lays down a <hash> dir with no ready marker (a half-built
// venv, or a pre-prune layout), so PruneVenvs must fall back to the
// directory's own mtime.
func seedVenvNoMarker(t *testing.T, sourceDir, hash string, at time.Time) string {
	t.Helper()
	dir := filepath.Join(sourceDir, hash)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir venv: %v", err)
	}
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatalf("chtimes dir: %v", err)
	}
	return dir
}

func mustExist(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected %s to survive prune: %v", dir, err)
	}
}

func mustNotExist(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected %s to be pruned, stat err = %v", dir, err)
	}
}

// TestPruneVenvsKeepsNewestNAndRecent is the core gate mirroring
// store.PruneTFStates's keep-both policy: newest keepN survive, anything at
// or after keepSince survives, and only what fails both is removed.
func TestPruneVenvsKeepsNewestNAndRecent(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "demo")
	now := time.Now()
	keepSince := now.Add(-30 * 24 * time.Hour)

	// Newest 3 by mtime, all older than keepSince — retained by keepN alone.
	n1 := seedVenv(t, sourceDir, "n1", now.Add(-1*time.Hour))
	n2 := seedVenv(t, sourceDir, "n2", now.Add(-2*time.Hour))
	n3 := seedVenv(t, sourceDir, "n3", now.Add(-3*time.Hour))
	// Old, but within keepSince — retained by recency alone.
	recent := seedVenv(t, sourceDir, "recent", now.Add(-1*time.Hour-29*24*time.Hour))
	// Old and outside keepN — must be pruned.
	stale1 := seedVenv(t, sourceDir, "stale1", now.Add(-40*24*time.Hour))
	stale2 := seedVenv(t, sourceDir, "stale2", now.Add(-90*24*time.Hour))

	n, err := PruneVenvs(root, "demo", 3, keepSince, "")
	if err != nil {
		t.Fatalf("PruneVenvs: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned = %d, want 2", n)
	}
	mustExist(t, n1)
	mustExist(t, n2)
	mustExist(t, n3)
	mustExist(t, recent)
	mustNotExist(t, stale1)
	mustNotExist(t, stale2)
}

// TestPruneVenvsProtectsGivenHash: a protected hash is never removed, even
// when it is both outside the newest-keepN window and older than keepSince —
// belt-and-braces beyond keepN for the venv currently in use.
func TestPruneVenvsProtectsGivenHash(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	sourceDir := filepath.Join(root, "demo")

	protected := seedVenv(t, sourceDir, "protected", now.Add(-365*24*time.Hour))
	seedVenv(t, sourceDir, "n1", now.Add(-1*time.Hour))
	seedVenv(t, sourceDir, "n2", now.Add(-2*time.Hour))
	seedVenv(t, sourceDir, "n3", now.Add(-3*time.Hour))

	n, err := PruneVenvs(root, "demo", 3, now.Add(-30*24*time.Hour), "protected")
	if err != nil {
		t.Fatalf("PruneVenvs: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned = %d, want 0 (protected is old and outside keepN)", n)
	}
	mustExist(t, protected)
}

// TestPruneVenvsKeepNBelowOneTreatedAsOne mirrors PruneTFStates: keepN<1 is
// clamped to 1, never zero.
func TestPruneVenvsKeepNBelowOneTreatedAsOne(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	sourceDir := filepath.Join(root, "demo")
	keepSince := now.Add(-30 * 24 * time.Hour)

	newest := seedVenv(t, sourceDir, "newest", now.Add(-40*24*time.Hour))
	older := seedVenv(t, sourceDir, "older", now.Add(-50*24*time.Hour))

	if _, err := PruneVenvs(root, "demo", 0, keepSince, ""); err != nil {
		t.Fatalf("PruneVenvs: %v", err)
	}
	mustExist(t, newest)
	mustNotExist(t, older)
}

// TestPruneVenvsRecencyFallsBackToDirMtime documents the recency signal: the
// ready marker's mtime wins when present; a marker-less venv falls back to
// the directory's own mtime rather than being treated as infinitely old (or
// new).
func TestPruneVenvsRecencyFallsBackToDirMtime(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	sourceDir := filepath.Join(root, "demo")
	keepSince := now.Add(-30 * 24 * time.Hour)

	// No marker, but the directory mtime is recent: must be kept by recency.
	halfBuilt := seedVenvNoMarker(t, sourceDir, "half-built", now.Add(-1*time.Hour))
	// Two unrelated, unambiguously-old marker-bearing venvs so keepN=1
	// forces the recency check to matter for half-built.
	seedVenv(t, sourceDir, "old1", now.Add(-40*24*time.Hour))
	seedVenv(t, sourceDir, "old2", now.Add(-50*24*time.Hour))

	if _, err := PruneVenvs(root, "demo", 1, keepSince, ""); err != nil {
		t.Fatalf("PruneVenvs: %v", err)
	}
	mustExist(t, halfBuilt)
}

// TestPruneVenvsMissingSourceIsNoop: a source with no cache dir yet (nothing
// has ever provisioned for it) is not an error.
func TestPruneVenvsMissingSourceIsNoop(t *testing.T) {
	root := t.TempDir()
	n, err := PruneVenvs(root, "never-seen", 3, time.Now(), "")
	if err != nil {
		t.Fatalf("PruneVenvs on missing source: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned = %d, want 0", n)
	}
}

// TestEnsureVenvPrunesStaleSiblingsAfterProvision is the integration gate for
// BUILD unit 2: a successful provision self-bounds the source's cache,
// removing stale siblings and protecting the venv it just built.
func TestEnsureVenvPrunesStaleSiblingsAfterProvision(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	uv := fakeUV(t, filepath.Join(t.TempDir(), "uv-invocations"))

	sourceDir := filepath.Join(cache, "demo")
	now := time.Now()
	// Two stale siblings, both well past keepSince, so keepN=1 must retain
	// only the newer of the two and prune the older — proving the prune ran
	// against this source's real siblings, not just the protected/no-op case.
	staleNewer := seedVenv(t, sourceDir, "stale-newer", now.Add(-40*24*time.Hour))
	staleOlder := seedVenv(t, sourceDir, "stale-older", now.Add(-90*24*time.Hour))

	m := manifestFor(t, project, "jmespath==1.1.0\n")
	spec := VenvSpec{
		CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv,
		KeepVenvs: 1, KeepVenvsSince: 24 * time.Hour,
	}

	v, err := EnsureVenv(context.Background(), spec)
	if err != nil {
		t.Fatalf("EnsureVenv: %v", err)
	}
	if !v.Created {
		t.Fatal("expected a fresh provision")
	}
	mustNotExist(t, staleOlder)
	mustExist(t, staleNewer)
	mustExist(t, v.Dir)
}

// TestPruneCollectionsKeepsNewestNAndRecent: the collections plane prunes with
// the identical keep-both policy as PruneVenvs — newest keepN by recency plus
// anything within keepSince survive, only what fails both is removed. The two
// share the sweep core (pruneHashDirs), so this mirrors
// TestPruneVenvsKeepsNewestNAndRecent.
func TestPruneCollectionsKeepsNewestNAndRecent(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "demo")
	now := time.Now()
	keepSince := now.Add(-30 * 24 * time.Hour)

	n1 := seedCollections(t, sourceDir, "n1", now.Add(-1*time.Hour))
	n2 := seedCollections(t, sourceDir, "n2", now.Add(-2*time.Hour))
	n3 := seedCollections(t, sourceDir, "n3", now.Add(-3*time.Hour))
	recent := seedCollections(t, sourceDir, "recent", now.Add(-1*time.Hour-29*24*time.Hour))
	stale1 := seedCollections(t, sourceDir, "stale1", now.Add(-40*24*time.Hour))
	stale2 := seedCollections(t, sourceDir, "stale2", now.Add(-90*24*time.Hour))

	n, err := PruneCollections(root, "demo", 3, keepSince, "")
	if err != nil {
		t.Fatalf("PruneCollections: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned = %d, want 2", n)
	}
	mustExist(t, n1)
	mustExist(t, n2)
	mustExist(t, n3)
	mustExist(t, recent)
	mustNotExist(t, stale1)
	mustNotExist(t, stale2)
}

// TestPruneCollectionsProtectsGivenHash: a protected hash is never removed,
// even when it is both outside the newest-keepN window and older than
// keepSince.
func TestPruneCollectionsProtectsGivenHash(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "demo")
	now := time.Now()

	protected := seedCollections(t, sourceDir, "protected", now.Add(-365*24*time.Hour))
	seedCollections(t, sourceDir, "n1", now.Add(-1*time.Hour))
	seedCollections(t, sourceDir, "n2", now.Add(-2*time.Hour))
	seedCollections(t, sourceDir, "n3", now.Add(-3*time.Hour))

	n, err := PruneCollections(root, "demo", 3, now.Add(-30*24*time.Hour), "protected")
	if err != nil {
		t.Fatalf("PruneCollections: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned = %d, want 0 (protected is old and outside keepN)", n)
	}
	mustExist(t, protected)
}

// TestPruneCollectionsMissingSourceIsNoop: a source with no collections cache
// dir yet is not an error.
func TestPruneCollectionsMissingSourceIsNoop(t *testing.T) {
	root := t.TempDir()
	n, err := PruneCollections(root, "never-seen", 3, time.Now(), "")
	if err != nil {
		t.Fatalf("PruneCollections on missing source: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned = %d, want 0", n)
	}
}

// TestEnsureVenvPruneFailureDoesNotFailProvision is the fail-safe gate: a
// prune error (here, a stale sibling RemoveAll cannot touch because its
// parent is read-only) must not turn a successful provision into a failure.
func TestEnsureVenvPruneFailureDoesNotFailProvision(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	uv := fakeUV(t, filepath.Join(t.TempDir(), "uv-invocations"))

	sourceDir := filepath.Join(cache, "demo")
	now := time.Now()
	// A newer decoy sibling fills the keepN=1 window, so the actually-stale
	// venv below is a genuine prune candidate rather than trivially retained.
	seedVenv(t, sourceDir, "stale-newer", now.Add(-40*24*time.Hour))
	stale := seedVenv(t, sourceDir, "stale-older", now.Add(-90*24*time.Hour))
	// Strip write permission on the stale venv's own dir so RemoveAll cannot
	// unlink its marker file — PruneVenvs then reports an error for it.
	if err := os.Chmod(stale, 0o500); err != nil {
		t.Fatalf("chmod stale venv: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stale, 0o700) })

	m := manifestFor(t, project, "jmespath==1.1.0\n")
	spec := VenvSpec{
		CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv,
		KeepVenvs: 1, KeepVenvsSince: 24 * time.Hour,
	}

	v, err := EnsureVenv(context.Background(), spec)
	if err != nil {
		t.Fatalf("EnsureVenv failed because of a prune error: %v", err)
	}
	if !v.Created {
		t.Fatal("expected a fresh provision")
	}
	// The stale venv survived because it could not be removed — the point of
	// the test is that this did not propagate as an EnsureVenv error.
	mustExist(t, stale)
}
