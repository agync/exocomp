package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestChunkSnapshotRoundTrip proves chunking and reassembly are
// byte-identical across a snapshot well over the 90 MB chunk size. The
// buffer is sparse (zero-filled) with a handful of distinct markers: cheap
// to build, and still enough to catch a swapped or misordered part.
func TestChunkSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	size := 2*int(chunkSize) + 12345
	sealed := make([]byte, size)
	for i, at := range []int{0, int(chunkSize) - 1, int(chunkSize), 2 * int(chunkSize), size - 1} {
		sealed[at] = byte(i + 1)
	}

	parts, manifest := chunkSnapshot("snapshot-roundtrip.age", sealed)
	if len(manifest.Parts) != 3 {
		t.Fatalf("parts = %d, want 3 for a %d byte snapshot chunked at %d", len(manifest.Parts), size, chunkSize)
	}
	if manifest.Parts[0].Size != chunkSize || manifest.Parts[1].Size != chunkSize {
		t.Errorf("full parts sized %d, %d; want %d each", manifest.Parts[0].Size, manifest.Parts[1].Size, chunkSize)
	}
	if want := int64(size) - 2*chunkSize; manifest.Parts[2].Size != want {
		t.Errorf("final part = %d bytes, want %d", manifest.Parts[2].Size, want)
	}
	if len(parts) != len(manifest.Parts) {
		t.Fatalf("wrote %d part files, manifest lists %d", len(parts), len(manifest.Parts))
	}

	dir := t.TempDir()
	for name, body := range parts {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, err := reassembleSnapshot(dir, manifest)
	if err != nil {
		t.Fatalf("reassembleSnapshot: %v", err)
	}
	if !bytes.Equal(got, sealed) {
		t.Fatal("reassembled snapshot is not byte-identical to the original")
	}
}

// TestChunkSnapshotSingleChunk proves a snapshot at or under chunkSize still
// gets exactly one part, so a restore has one reassembly path regardless of
// size.
func TestChunkSnapshotSingleChunk(t *testing.T) {
	t.Parallel()

	sealed := bytes.Repeat([]byte("exocomp-chunk-fixture-"), 256)
	parts, manifest := chunkSnapshot("snapshot-small.age", sealed)
	if len(parts) != 1 || len(manifest.Parts) != 1 {
		t.Fatalf("parts = %d/%d, want 1/1 for a %d byte snapshot", len(parts), len(manifest.Parts), len(sealed))
	}
	if manifest.Parts[0].Name != "snapshot-small.age.part00" {
		t.Errorf("part name = %s, want snapshot-small.age.part00", manifest.Parts[0].Name)
	}
}

func TestReassembleSnapshotDetectsTamperedPart(t *testing.T) {
	t.Parallel()

	sealed := bytes.Repeat([]byte("exocomp-chunk-fixture-"), 4096)
	parts, manifest := chunkSnapshot("snapshot-tamper.age", sealed)

	dir := t.TempDir()
	for name, body := range parts {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if _, err := reassembleSnapshot(dir, manifest); err != nil {
		t.Fatalf("reassemble before tamper: %v", err)
	}

	name := manifest.Parts[0].Name
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	body[0] ^= 0xff
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatalf("rewrite %s: %v", name, err)
	}

	if _, err := reassembleSnapshot(dir, manifest); err == nil {
		t.Fatal("reassembleSnapshot accepted a tampered part")
	}
}

func TestReassembleSnapshotDetectsMissingPart(t *testing.T) {
	t.Parallel()

	parts, manifest := chunkSnapshot("snapshot-missing.age", []byte("small snapshot"))
	dir := t.TempDir()
	for name, body := range parts {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, manifest.Parts[0].Name)); err != nil {
		t.Fatalf("remove part: %v", err)
	}
	if _, err := reassembleSnapshot(dir, manifest); err == nil {
		t.Fatal("reassembleSnapshot accepted a missing part")
	}
}

func TestPartsManifestRoundTrip(t *testing.T) {
	t.Parallel()

	_, manifest := chunkSnapshot("snapshot-manifest.age", []byte("payload"))
	dir := t.TempDir()
	if err := writePartsManifest(dir, manifest); err != nil {
		t.Fatalf("writePartsManifest: %v", err)
	}
	got, err := readPartsManifest(dir, "snapshot-manifest.age")
	if err != nil {
		t.Fatalf("readPartsManifest: %v", err)
	}
	if got.SHA256 != manifest.SHA256 || got.Size != manifest.Size || len(got.Parts) != len(manifest.Parts) {
		t.Errorf("read back %+v, want %+v", got, manifest)
	}
}

// TestGitLegChunksAcrossMultipleParts shrinks chunkSize so the leg's tiny
// staging tree still splits into several parts, proving the wiring through
// gitLeg and restoreGit without needing a genuinely huge fixture.
func TestGitLegChunksAcrossMultipleParts(t *testing.T) {
	requireTool(t, "git")
	f := newFixture(t)

	old := chunkSize
	chunkSize = 256
	t.Cleanup(func() { chunkSize = old })

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "init", "--bare", "--quiet", bare)
	target := f.target(t, "offsite-git-chunked", KindGit, bare)

	report, err := f.engine.Run(t.Context(), []Target{target})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("git leg: %v", err)
	}

	tree := gitRun(t, bare, "ls-tree", "--name-only", backupRef)
	var parts int
	for _, n := range strings.Fields(tree) {
		if strings.Contains(n, ".age.part") {
			parts++
		}
	}
	if parts < 2 {
		t.Fatalf("committed %d part(s) at chunkSize=%d, want at least 2", parts, chunkSize)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreLatest(t.Context(), Source{
		Kind:         KindGit,
		URI:          bare,
		IdentityFile: f.identity,
	}, dest); err != nil {
		t.Fatalf("restore chunked snapshot: %v", err)
	}
	assertRestoredChain(t, f, dest, "offsite-git-chunked")
}

// TestGitLegRefusesOversizedRepo fakes du rather than actually growing a
// repository to 4 GB in a test.
func TestGitLegRefusesOversizedRepo(t *testing.T) {
	requireTool(t, "git")
	f := newFixture(t)

	old := repoDU
	repoDU = func(context.Context, string) (int64, error) {
		return repoSizeGuard + 1, nil
	}
	t.Cleanup(func() { repoDU = old })

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "init", "--bare", "--quiet", bare)
	target := f.target(t, "offsite-git-oversized", KindGit, bare)

	report, err := f.engine.Run(t.Context(), []Target{target})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := report.Err(); err == nil {
		t.Fatal("git leg accepted a repo over the size guard")
	}
	if report.Legs[0].Err == nil || !strings.Contains(report.Legs[0].Err.Error(), "byte guard") {
		t.Errorf("error = %v, want the size guard's message", report.Legs[0].Err)
	}

	runs, err := f.store.ListBackupRuns(t.Context(), target.ID, 1)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].State != store.BackupFailed || !strings.Contains(runs[0].Error, "byte guard") {
		t.Fatalf("backup_run = %+v, want a failed row recording the size guard", runs)
	}
}

// TestCheckRepoSizeIgnoresUnmeasurableRepo proves the guard defers to the
// push itself for a target it cannot measure, rather than refusing a leg for
// a reason unrelated to size.
func TestCheckRepoSizeIgnoresUnmeasurableRepo(t *testing.T) {
	t.Parallel()

	if err := checkRepoSize(t.Context(), filepath.Join(t.TempDir(), "does-not-exist.git")); err != nil {
		t.Errorf("checkRepoSize on a nonexistent repo = %v, want nil", err)
	}
}
