package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// TestStageNeverCapturesTheRunSecretStagingDir pins the disk-based
// secret-staging root's exclusion from backup, the finding Change 3 confirms
// rather than implements: Engine.stage builds the tree it hands to every leg
// from exactly the files it writes itself here — VacuumInto's snapshot.db, the
// keyring, its version, the manifest — never by walking the data dir. A
// disk-staged run-secret sitting under "<data-dir>/run/" (cmd/exocomp's
// --secret-staging disk backend root) is therefore already outside the
// captured set by construction; there is no exclude list to add, and this
// test is what would catch a regression that started walking the data dir
// instead of writing named files into it.
func TestStageNeverCapturesTheRunSecretStagingDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Stage a disk-backed run-secret under the same "<data-dir>/run/" tree
	// cmd/exocomp's disk backend uses, sitting right next to snapshot.db's own
	// data dir — the case that would fail if the backup ever started sweeping
	// the data dir wholesale instead of writing its four named files.
	diskRoot := filepath.Join(f.dataDir, "run")
	if err := os.MkdirAll(diskRoot, 0o700); err != nil {
		t.Fatalf("mkdir disk root: %v", err)
	}
	staging, err := executor.ResolveStaging(executor.StagingConfig{
		Backend: executor.StagingDisk, DiskRoot: diskRoot,
	})
	if err != nil {
		t.Fatalf("resolve staging: %v", err)
	}
	const secretSentinel = "backup-must-never-see-this-9f2c"
	_, staged, err := staging.StageFiles(
		executor.RunSecrets{"KUBECONFIG": secretSentinel}, []string{"KUBECONFIG"})
	if err != nil {
		t.Fatalf("stage run-secret: %v", err)
	}
	t.Cleanup(func() { _ = staged[0].Close() })

	stageDir := t.TempDir()
	if _, err := f.engine.stage(t.Context(), stageDir); err != nil {
		t.Fatalf("stage: %v", err)
	}

	entries, err := os.ReadDir(stageDir)
	if err != nil {
		t.Fatalf("read staged tree: %v", err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
		if strings.Contains(e.Name(), "exocomp-secret-") || strings.HasPrefix(e.Name(), "exocomp.") {
			t.Errorf("the backup staging tree captured a run-secret staging artifact: %s", e.Name())
		}
	}
	for _, want := range []string{SnapshotName, KeyringName, KeyringVersionName} {
		if !names[want] {
			t.Errorf("staged tree is missing %s: %v", want, names)
		}
	}

	// The staged snapshot.db itself does not carry the secret either — proof
	// that no incidental path (a WAL checkpoint, a stray copy) leaked it in
	// through the one file that IS captured.
	snapshot, err := os.ReadFile(filepath.Join(stageDir, SnapshotName))
	if err != nil {
		t.Fatalf("read staged snapshot: %v", err)
	}
	if strings.Contains(string(snapshot), secretSentinel) {
		t.Error("the staged database snapshot carries the run-secret sentinel")
	}
}
