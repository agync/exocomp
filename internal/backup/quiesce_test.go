package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/store"
)

// readTrim reads a file and trims trailing whitespace, for the ordered-marker
// assertions below.
func readTrim(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(body))
}

// TestQuiesceThawsAfterBackup is the happy path and the mutation counterpart to
// the abort test below: a passing quiesce lets the snapshot through, a leg runs,
// and the source is un-quiesced afterwards. The shared log proves the order:
// quiesce ran, then un-quiesce, around a snapshot that actually happened.
func TestQuiesceThawsAfterBackup(t *testing.T) {
	f := newFixture(t)
	repo := t.TempDir()
	target := f.target(t, "local", KindResticLocal, repo)

	log := filepath.Join(t.TempDir(), "quiesce.log")
	q := &Quiesce{
		Pre:  []hooks.Hook{{Name: "quiesce", Run: "echo quiesce >> " + log}},
		Post: []hooks.Hook{{Name: "thaw", Run: "echo thaw >> " + log}},
	}
	eng := New(f.store, f.env, Options{Quiesce: q, StageRoot: t.TempDir()})

	rep, err := eng.Run(t.Context(), []Target{target})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Legs) != 1 || rep.Legs[0].State != store.BackupSucceeded {
		t.Fatalf("expected one succeeded leg, got %+v", rep.Legs)
	}
	// A snapshot was actually taken (mutation guard: the abort test's "no leg"
	// is only meaningful because this one produces a leg).
	if rep.Legs[0].SnapshotID == "" {
		t.Fatalf("leg reported no snapshot id: %+v", rep.Legs[0])
	}
	if got := readTrim(t, log); got != "quiesce\nthaw" {
		t.Fatalf("quiesce log = %q, want quiesce then thaw around the snapshot", got)
	}
}

// TestQuiesceAbortsOnFailedPreHook proves the safety contract: a failing
// quiesce step aborts the backup before any snapshot is assembled (no leg runs,
// no backup_run row), and the source is un-quiesced on the way out so it is not
// left frozen.
func TestQuiesceAbortsOnFailedPreHook(t *testing.T) {
	f := newFixture(t)
	repo := t.TempDir()
	target := f.target(t, "local", KindResticLocal, repo)

	log := filepath.Join(t.TempDir(), "quiesce.log")
	q := &Quiesce{
		Pre: []hooks.Hook{
			{Name: "pg-dump", Run: "echo quiesce >> " + log},
			{Name: "verify-dump", Run: "echo failing >> " + log + "; exit 1"},
		},
		Post: []hooks.Hook{{Name: "thaw", Run: "echo thaw >> " + log}},
	}
	eng := New(f.store, f.env, Options{Quiesce: q, StageRoot: t.TempDir()})

	rep, err := eng.Run(t.Context(), []Target{target})
	if err == nil {
		t.Fatal("expected the backup to abort on a failed quiesce step")
	}
	if !strings.Contains(err.Error(), "quiesce aborted before any snapshot") {
		t.Fatalf("abort error does not name the aborted quiesce: %v", err)
	}
	// Never a torn snapshot: no leg ran, so no backup_run row exists yet.
	if len(rep.Legs) != 0 {
		t.Fatalf("a leg ran despite the aborted quiesce: %+v", rep.Legs)
	}
	runs, err := f.store.ListBackupRuns(t.Context(), target.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("a backup_run row was written despite the abort: %+v", runs)
	}
	// The failing quiesce still un-quiesced: quiesce started, hit the failure,
	// then thaw ran, so the source is not left frozen.
	if got := readTrim(t, log); got != "quiesce\nfailing\nthaw" {
		t.Fatalf("quiesce log = %q, want the failed quiesce followed by an un-quiesce", got)
	}
}

// TestLoadQuiesceRefusesAGate: a backup quiesce has only pre/post steps; a gate
// is meaningless here and must be refused, not silently dropped.
func TestLoadQuiesceRefusesAGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiesce.yml")
	if err := os.WriteFile(path, []byte("pre_run:\n  - name: q\n    run: \"true\"\n"+
		"gate:\n  - name: web\n    http:\n      url: http://127.0.0.1:9/\n"), 0o600); err != nil {
		t.Fatalf("write quiesce: %v", err)
	}
	if _, err := LoadQuiesce(path); err == nil {
		t.Fatal("LoadQuiesce accepted a quiesce file that declares a gate")
	}
}

// TestLoadQuiesceMissingIsNoQuiesce: a missing file means the backup runs
// exactly as before, unquiesced.
func TestLoadQuiesceMissingIsNoQuiesce(t *testing.T) {
	q, err := LoadQuiesce(filepath.Join(t.TempDir(), "absent.yml"))
	if err != nil {
		t.Fatalf("LoadQuiesce on a missing file errored: %v", err)
	}
	if q != nil {
		t.Fatalf("a missing quiesce file yielded a non-nil Quiesce: %+v", q)
	}
}
