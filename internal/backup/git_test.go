package backup

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitLegRollsOneOrphanCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real git")
	}
	t.Parallel()
	requireTool(t, "git")
	f := newFixture(t)

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "init", "--bare", "--quiet", bare)
	target := f.target(t, "offsite-git", KindGit, bare)

	// Two runs an hour apart: the ref must still hold exactly one commit, and
	// it must be the newer snapshot.
	first := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	for _, at := range []time.Time{first, second} {
		engine := New(f.store, f.env, Options{Now: func() time.Time { return at }})
		report, err := engine.Run(t.Context(), []Target{target})
		if err != nil {
			t.Fatalf("run at %s: %v", at, err)
		}
		if err := report.Err(); err != nil {
			t.Fatalf("git leg at %s: %v", at, err)
		}
		if len(report.Legs[0].SnapshotID) != 40 {
			t.Errorf("snapshot id = %q, want a commit sha", report.Legs[0].SnapshotID)
		}
	}

	if got := gitRun(t, bare, "rev-list", "--count", backupRef, "--"); got != "1" {
		t.Errorf("commits on %s = %s, want 1 (the leg must force-push an orphan)", backupRef, got)
	}
	if got := gitRun(t, bare, "rev-list", "--count", "--all", "--"); got != "1" {
		t.Errorf("reachable commits in the repo = %s, want 1", got)
	}

	refs := gitRun(t, bare, "for-each-ref", "--format=%(refname)")
	if refs != backupRef {
		t.Errorf("refs = %q, want only %s", refs, backupRef)
	}

	tree := gitRun(t, bare, "ls-tree", "--name-only", backupRef)
	names := strings.Fields(tree)
	wantSnapshot := "snapshot-" + second.Format(stampLayout)
	var havePart, haveManifest bool
	for _, n := range names {
		if !strings.HasPrefix(n, wantSnapshot) {
			continue
		}
		switch {
		case strings.HasSuffix(n, partsSuffix):
			haveManifest = true
		case strings.Contains(n, ".age.part"):
			havePart = true
		}
	}
	if !havePart {
		t.Errorf("tree = %v, want a %s*.age.part* chunk", names, wantSnapshot)
	}
	if !haveManifest {
		t.Errorf("tree = %v, want the %s%s manifest", names, wantSnapshot, partsSuffix)
	}
	for _, want := range []string{KeyringName, RecoveryName} {
		if !strings.Contains(tree, want) {
			t.Errorf("tree = %v, want %s committed alongside", names, want)
		}
	}
	// The stale snapshot from the first run must be gone with its commit.
	if strings.Contains(tree, "snapshot-"+first.Format(stampLayout)) {
		t.Error("the first run's archive is still on the ref")
	}

	dest := filepath.Join(t.TempDir(), "restored")
	manifest, err := RestoreLatest(t.Context(), Source{
		Kind:         KindGit,
		URI:          bare,
		IdentityFile: f.identity,
	}, dest)
	if err != nil {
		t.Fatalf("restore from git: %v", err)
	}
	if !manifest.CreatedAt.Equal(second) {
		t.Errorf("restored manifest taken at %s, want %s", manifest.CreatedAt, second)
	}
	assertRestoredChain(t, f, dest, "offsite-git")
}

func TestGitSnapshotIsUnreadableWithoutAnIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real git")
	}
	t.Parallel()
	requireTool(t, "git")
	f := newFixture(t)

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "init", "--bare", "--quiet", bare)
	target := f.target(t, "offsite-git", KindGit, bare)
	if _, err := f.engine.Run(t.Context(), []Target{target}); err != nil {
		t.Fatalf("run: %v", err)
	}

	_, err := RestoreLatest(t.Context(), Source{Kind: KindGit, URI: bare},
		filepath.Join(t.TempDir(), "restored"))
	if err == nil {
		t.Fatal("restored a git snapshot without an age identity")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Errorf("error = %v, want a missing-identity error", err)
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real git")
	}
	t.Parallel()
	f := newFixture(t)

	stage := t.TempDir()
	if _, err := f.engine.stage(t.Context(), stage); err != nil {
		t.Fatalf("stage: %v", err)
	}

	archive, ext, err := archiveDir(t.Context(), stage)
	if err != nil {
		t.Fatalf("archiveDir: %v", err)
	}
	if _, err := exec.LookPath("zstd"); err == nil && ext != ".tar.zst" {
		t.Errorf("extension = %s, want .tar.zst when zstd is installed", ext)
	}

	raw, err := decompress(t.Context(), archive)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	out := t.TempDir()
	if err := untar(raw, out); err != nil {
		t.Fatalf("untar: %v", err)
	}
	manifest, err := ReadManifest(out)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if err := manifest.Verify(out); err != nil {
		t.Fatalf("verify unpacked archive: %v", err)
	}
}

// gitRun runs git with the same explicit environment the leg uses, so an
// inherited GIT_* variable — the pre-commit hook exports GIT_INDEX_FILE, for
// one — cannot decide what these assertions see.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", full...)
	cmd.Env = gitEnv(nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s\nrefs: %s", strings.Join(full, " "), err, out, gitRefs(dir))
	}
	return strings.TrimSpace(string(out))
}

func gitRefs(dir string) string {
	cmd := exec.Command("git", "-C", dir, "for-each-ref", "--format=%(refname) %(objectname)")
	cmd.Env = gitEnv(nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "for-each-ref failed: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}
