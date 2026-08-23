package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// snapshotDataDir returns the name and size of every entry directly under
// dir, sorted, so a before/after comparison catches any write into it.
func snapshotDataDir(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	got := make(map[string]int64, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		got[e.Name()] = info.Size()
	}
	return got
}

// TestRestoreDrillFindsNewestSnapshot: a drill against a real restic target
// restores, verifies, and opens the newest snapshot, reports the right
// snapshot id and a plausible RPO age, leaves the live data dir untouched,
// and cleans up its own throwaway directory.
func TestRestoreDrillFindsNewestSnapshot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	repo := filepath.Join(t.TempDir(), "nas")
	target := f.target(t, "nas", KindResticLocal, repo)
	if _, err := f.engine.Run(t.Context(), []Target{target}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	before := snapshotDataDir(t, f.dataDir)
	stageRoot := t.TempDir()

	result, err := RestoreDrill(t.Context(), Source{
		Kind:         KindResticLocal,
		URI:          repo,
		IdentityFile: f.identity,
		Env:          map[string]string{"RESTIC_CACHE_DIR": f.cache},
	}, stageRoot)
	if err != nil {
		t.Fatalf("drill: %v", err)
	}

	if !result.OK {
		t.Fatalf("drill result not OK: %+v", result)
	}
	if result.Reason != "" {
		t.Errorf("OK result carries a reason: %q", result.Reason)
	}
	if result.Source != KindResticLocal+":"+repo {
		t.Errorf("source = %q, want %s:%s", result.Source, KindResticLocal, repo)
	}

	stamp, err := time.Parse(time.RFC3339, result.SnapshotID)
	if err != nil {
		t.Fatalf("snapshot id %q does not parse as RFC3339: %v", result.SnapshotID, err)
	}
	if age := time.Since(stamp); age < 0 || age > time.Minute {
		t.Errorf("snapshot id %q is not a plausible just-taken timestamp (age %s)", result.SnapshotID, age)
	}
	if result.RPOAge < 0 || result.RPOAge > time.Minute {
		t.Errorf("RPO age = %s, want a small positive duration", result.RPOAge)
	}
	if result.VerifiedFiles != 3 {
		t.Errorf("verified files = %d, want 3", result.VerifiedFiles)
	}
	if result.VerifiedBytes <= 0 {
		t.Errorf("verified bytes = %d, want > 0", result.VerifiedBytes)
	}

	after := snapshotDataDir(t, f.dataDir)
	if len(before) != len(after) {
		t.Fatalf("live data dir entry count changed: %v -> %v", before, after)
	}
	for name, size := range before {
		if after[name] != size {
			t.Errorf("live data dir entry %s changed size %d -> %d: the drill touched the live dir", name, size, after[name])
		}
	}
	if _, err := f.store.GetSecret(t.Context(), "canary"); err != nil {
		t.Errorf("live store unusable after the drill: %v", err)
	}

	entries, err := os.ReadDir(stageRoot)
	if err != nil {
		t.Fatalf("read stage root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("drill left %d entries under its stage root: temp dir was not cleaned up", len(entries))
	}
}

// corruptingRestoreDriver runs every subprocess for real, then flips the
// bytes of SnapshotName wherever a "restic ... restore ... --target <dir>"
// call just wrote it — simulating a backup that is present but corrupt,
// without needing to forge restic's own on-disk repository format.
type corruptingRestoreDriver struct{ inner executor.Driver }

func (d corruptingRestoreDriver) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	res, err := d.inner.Run(ctx, spec, sink)
	if err != nil || res.ExitCode != 0 || !slices.Contains(spec.Argv, "restore") {
		return res, err
	}
	i := slices.Index(spec.Argv, "--target")
	if i < 0 || i+1 >= len(spec.Argv) {
		return res, err
	}
	target := spec.Argv[i+1]
	_ = filepath.WalkDir(target, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == SnapshotName {
			if writeErr := os.WriteFile(path, []byte("not a database, corrupted by the test"), 0o600); writeErr != nil {
				return writeErr
			}
			return fs.SkipAll
		}
		return nil
	})
	return res, err
}

// TestRestoreDrillCatchesCorruption: a snapshot whose restored database no
// longer matches its manifest comes back as DrillResult{OK: false} with a
// clear reason — not a thrown error. The drill did its job: it found a bad
// backup.
func TestRestoreDrillCatchesCorruption(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	repo := filepath.Join(t.TempDir(), "nas")
	target := f.target(t, "nas", KindResticLocal, repo)
	if _, err := f.engine.Run(t.Context(), []Target{target}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	result, err := RestoreDrill(t.Context(), Source{
		Kind:         KindResticLocal,
		URI:          repo,
		IdentityFile: f.identity,
		Env:          map[string]string{"RESTIC_CACHE_DIR": f.cache},
		Driver:       corruptingRestoreDriver{inner: executor.NewShell()},
	}, t.TempDir())
	if err != nil {
		t.Fatalf("a corrupt backup must be reported, not returned as an error: %v", err)
	}
	if result.OK {
		t.Fatal("drill reported OK against a corrupted snapshot")
	}
	if result.Reason == "" {
		t.Error("OK:false result carries no reason")
	}
	if !strings.Contains(result.Reason, SnapshotName) {
		t.Errorf("reason %q does not name the corrupted file", result.Reason)
	}
}

// TestRestoreDrillNoBackupIsAnError: a target that was never backed up has
// no snapshot to inspect at all — the drill cannot run, so it returns a real
// error rather than a DrillResult.
func TestRestoreDrillNoBackupIsAnError(t *testing.T) {
	t.Parallel()
	requireTool(t, "restic")

	stageRoot := t.TempDir()
	result, err := RestoreDrill(t.Context(), Source{
		Kind: KindResticLocal,
		URI:  filepath.Join(t.TempDir(), "never-backed-up"),
	}, stageRoot)
	if err == nil {
		t.Fatal("drill against a target that was never backed up returned no error")
	}
	if isDrillFinding(err) {
		t.Errorf("a no-backup-available error was classified as a drill finding: %v", err)
	}
	if (result != DrillResult{}) {
		t.Errorf("error result is not zero-valued: %+v", result)
	}

	entries, err := os.ReadDir(stageRoot)
	if err != nil {
		t.Fatalf("read stage root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("drill left %d entries under its stage root on the error path", len(entries))
	}
}

// TestIsDrillFinding pins the classification a real snapshot corruption must
// hit: Manifest.Verify's own error, wrapped by ErrSnapshotCorrupt.
func TestIsDrillFinding(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{Files: []FileEntry{{Name: "missing", SHA256: "x", Size: 1}}}
	err := m.Verify(dir)
	if err == nil {
		t.Fatal("verify of a manifest naming a missing file should fail")
	}
	if !errors.Is(err, ErrSnapshotCorrupt) {
		t.Errorf("verify error does not wrap ErrSnapshotCorrupt: %v", err)
	}
	if !isDrillFinding(err) {
		t.Errorf("isDrillFinding(%v) = false, want true", err)
	}
	if isDrillFinding(errors.New("backup: some infrastructure failure")) {
		t.Error("an unrelated plain error was classified as a drill finding")
	}
}
