package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/tabwriter"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// drillFixture seeds a fully initialized data dir (envelope + host key,
// resolved the way the CLI itself resolves it) and returns the pieces a test
// needs to also seed a real backup target directly through the engine, the
// same way internal/backup's own fixtures do — `backup drill` is exercised
// through the real CLI command below; only the setup takes the shortcut.
type drillFixture struct {
	dataDir string
	env     *secrets.Envelope
	cache   string
}

func setupDrillFixture(t *testing.T) drillFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	// restic_local's cache falls back to $HOME/.cache/restic (the CLI's own
	// targetEnv has no override for it, unlike the private cache the
	// internal/backup fixtures use for seeding below) — a scratch HOME keeps
	// the real `backup drill` invocation from writing into the operator's
	// own cache.
	t.Setenv("HOME", t.TempDir())

	dataDir := filepath.Join(root, "exocomp")
	env, _, err := secrets.Init(dataDir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })

	return drillFixture{dataDir: dataDir, env: env, cache: t.TempDir()}
}

// seedBackup writes one real restic_local snapshot straight through
// internal/backup's engine — the same chain `backup run` uses — so `backup
// drill` has real bytes to restore and verify.
func (f drillFixture) seedBackup(t *testing.T, name, repo string) store.BackupTarget {
	t.Helper()
	st, err := store.Open(filepath.Join(f.dataDir, "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	target, err := st.CreateBackupTarget(context.Background(), store.BackupTarget{
		Name: name, Kind: backup.KindResticLocal, URI: repo, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	eng := backup.New(st, f.env, backup.Options{})
	report, err := eng.Run(context.Background(), []backup.Target{
		{BackupTarget: target, Env: map[string]string{"RESTIC_CACHE_DIR": f.cache}},
	})
	if err != nil || report.Err() != nil {
		t.Fatalf("seed backup: run err=%v, report err=%v", err, report.Err())
	}
	return target
}

// snapshotDir returns the name and size of every entry directly under dir,
// so a before/after comparison catches any write `backup drill` makes into
// the live data dir.
func snapshotDir(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
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

// TestBackupDrillHealthyTargetPrintsOK is the CLI verb against a real seeded
// local restic target: it prints OK with a plausible RPO, exits 0, and
// leaves the live data dir untouched.
func TestBackupDrillHealthyTargetPrintsOK(t *testing.T) {
	requireTools(t, "restic", "age")
	f := setupDrillFixture(t)
	repo := filepath.Join(t.TempDir(), "nas")
	f.seedBackup(t, "nas", repo)

	before := snapshotDir(t, f.dataDir)

	out, err := run(t, "backup", "drill")
	if err != nil {
		t.Fatalf("backup drill: %v\n%s", err, out)
	}
	t.Logf("backup drill output:\n%s", out)

	if !strings.Contains(out, "nas") || !strings.Contains(out, "OK") {
		t.Errorf("output does not report target %q OK:\n%s", "nas", out)
	}

	after := snapshotDir(t, f.dataDir)
	if len(before) != len(after) {
		t.Fatalf("live data dir entry count changed: %v -> %v", before, after)
	}
	for name, size := range before {
		if after[name] != size {
			t.Errorf("live data dir entry %s changed size %d -> %d: the drill touched the live dir", name, size, after[name])
		}
	}
}

// TestBackupDrillJSONOutputEmitsEnvelope covers --output json: the wire
// envelope names kind backup.drill and carries a structured, OK finding for
// the seeded target.
func TestBackupDrillJSONOutputEmitsEnvelope(t *testing.T) {
	requireTools(t, "restic", "age")
	f := setupDrillFixture(t)
	repo := filepath.Join(t.TempDir(), "nas")
	f.seedBackup(t, "nas", repo)

	out, err := run(t, "backup", "drill", "--output", "json")
	if err != nil {
		t.Fatalf("backup drill --output json: %v\n%s", err, out)
	}
	t.Logf("backup drill --output json:\n%s", out)

	var envelope struct {
		SchemaVersion int              `json:"schema_version"`
		Kind          string           `json:"kind"`
		Data          []wire.DrillView `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, out)
	}
	if envelope.Kind != wire.KindBackupDrill {
		t.Errorf("kind = %q, want %q", envelope.Kind, wire.KindBackupDrill)
	}
	if len(envelope.Data) != 1 {
		t.Fatalf("data = %d entries, want 1", len(envelope.Data))
	}
	view := envelope.Data[0]
	if view.Target != "nas" || !view.OK || view.SnapshotID == "" || view.VerifiedFiles == 0 {
		t.Errorf("view = %+v, want an OK finding for nas with a snapshot id and verified files", view)
	}
}

// TestBackupDrillCorruptedTargetFailsLoudly: a restic repository damaged on
// disk after a real backup is a real "corrupted backup" — the drill cannot
// complete a trustworthy restore from it, and that is reported clearly and
// exits nonzero rather than being swallowed. The live data dir stays
// untouched either way.
func TestBackupDrillCorruptedTargetFailsLoudly(t *testing.T) {
	requireTools(t, "restic", "age")
	f := setupDrillFixture(t)
	repo := filepath.Join(t.TempDir(), "nas")
	f.seedBackup(t, "nas", repo)
	corruptResticRepo(t, repo)

	before := snapshotDir(t, f.dataDir)

	out, err := run(t, "backup", "drill")
	if err == nil {
		t.Fatalf("backup drill against a corrupted repository: want a nonzero exit\n%s", out)
	}
	t.Logf("backup drill exit error: %v\noutput:\n%s", err, out)

	if !strings.Contains(out, "nas") {
		t.Errorf("output does not name the failing target:\n%s", out)
	}

	after := snapshotDir(t, f.dataDir)
	if len(before) != len(after) {
		t.Fatalf("live data dir entry count changed: %v -> %v", before, after)
	}
	for name, size := range before {
		if after[name] != size {
			t.Errorf("live data dir entry %s changed size %d -> %d: the drill touched the live dir", name, size, after[name])
		}
	}
}

// TestDrillFindingReportsFailAndNonzero pins the plumbing a synthetic
// DrillResult{OK: false} drives, without needing a real corrupted restic
// round trip (that chain is covered end to end by
// TestBackupDrillCorruptedTargetFailsLoudly and, at the package level, by
// internal/backup's own TestRestoreDrillCatchesCorruption): the text row
// reads FAIL with the reason, the JSON view carries ok:false and the reason,
// and the outcome joins into a non-nil error — the one that decides the
// process exit code.
func TestDrillFindingReportsFailAndNonzero(t *testing.T) {
	oc := drillOutcome{
		target: store.BackupTarget{Name: "nas", Kind: backup.KindResticLocal, URI: "/repo"},
		result: backup.DrillResult{
			Source: "restic_local:/repo", OK: false,
			Reason: "restored database failed PRAGMA integrity_check: malformed",
		},
	}

	view := drillView(oc)
	if view.OK || view.Reason == "" || view.Error != "" {
		t.Fatalf("view = %+v", view)
	}

	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	writeDrillRow(w, oc)
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	row := buf.String()
	if !strings.Contains(row, "nas") || !strings.Contains(row, "FAIL") || !strings.Contains(row, "integrity_check") {
		t.Errorf("row = %q", row)
	}

	if err := drillOutcomeErr([]drillOutcome{oc}); err == nil {
		t.Error("drillOutcomeErr for a finding: want a non-nil error")
	}
}

// corruptResticRepo mangles the bytes of every pack object under a
// restic_local repository's data/ tree, simulating a backup damaged at rest
// (bit rot, a failed disk, a bad copy) without needing to forge restic's own
// repository format.
func corruptResticRepo(t *testing.T, repo string) {
	t.Helper()
	dataDir := filepath.Join(repo, "data")
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read repo data dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("repo data dir is empty; nothing to corrupt")
	}
	corrupted := 0
	for _, prefix := range entries {
		if !prefix.IsDir() {
			continue
		}
		sub := filepath.Join(dataDir, prefix.Name())
		blobs, err := os.ReadDir(sub)
		if err != nil {
			t.Fatalf("read %s: %v", sub, err)
		}
		for _, blob := range blobs {
			path := filepath.Join(sub, blob.Name())
			// restic writes pack objects read-only; reclaim write access
			// before overwriting one.
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatalf("chmod %s: %v", path, err)
			}
			if err := os.WriteFile(path, []byte("not a valid restic pack, corrupted by the test"), 0o600); err != nil {
				t.Fatalf("corrupt %s: %v", path, err)
			}
			corrupted++
		}
	}
	if corrupted == 0 {
		t.Fatal("found no pack objects to corrupt")
	}
}
