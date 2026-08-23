package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func runnerFixture() Runner {
	return Runner{
		Name:        "fedora-ee",
		Mode:        RunnerModeCached,
		Base:        "registry.fedoraproject.org/fedora-minimal:44",
		Bootstrap:   "microdnf install -y sshpass rsync\n",
		Packages:    []string{"rsync", "sshpass"},
		AnsibleCore: "2.18.1",
		MemoryMax:   "2G",
		CPUQuota:    "200%",
	}
}

// TestMigration0018UpgradesFrom17 pins a database at user_version 17, then
// opens it with the current binary: the migration must apply and the new table
// must be usable through the CRUD, which is what an operator upgrading in place
// gets.
func TestMigration0018UpgradesFrom17(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, dbPath, 17)

	raw, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var count int
	err = raw.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'runner'`).Scan(&count)
	if err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	if count != 0 {
		t.Fatalf("runner table exists at user_version 17, want it introduced by 0018")
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open at v17 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 18 {
		t.Fatalf("schema version = %d after open, want >= 18", version)
	}

	if _, err := s.CreateRunner(ctx, runnerFixture()); err != nil {
		t.Fatalf("create runner on migrated schema: %v", err)
	}
	rows, err := s.ListRunners(ctx)
	if err != nil {
		t.Fatalf("list runners: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "fedora-ee" {
		t.Fatalf("list = %+v, want the one created runner", rows)
	}
}

func TestRunnerCRUDRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	// Packages go in unsorted and duplicated; they come back canonical.
	in := runnerFixture()
	in.Packages = []string{"sshpass", "rsync", "sshpass", " "}
	created, err := s.CreateRunner(ctx, in)
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	if got := created.Packages; len(got) != 2 || got[0] != "rsync" || got[1] != "sshpass" {
		t.Errorf("packages = %v, want canonical [rsync sshpass]", got)
	}
	if created.ContentHash == "" || len(created.ContentHash) != 32 {
		t.Errorf("content hash = %q, want 32 hex characters", created.ContentHash)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stamped: %+v", created)
	}

	byName, err := s.RunnerByName(ctx, "fedora-ee")
	if err != nil {
		t.Fatalf("runner by name: %v", err)
	}
	byID, err := s.GetRunner(ctx, created.ID)
	if err != nil {
		t.Fatalf("get runner: %v", err)
	}
	if byName.ContentHash != created.ContentHash || byID.ContentHash != created.ContentHash {
		t.Errorf("hash drifts across reads: create=%q name=%q id=%q",
			created.ContentHash, byName.ContentHash, byID.ContentHash)
	}
	if byName.Bootstrap != in.Bootstrap {
		t.Errorf("bootstrap = %q, want %q", byName.Bootstrap, in.Bootstrap)
	}

	// A definition change moves the hash; a mode change alone does not.
	sameRecipe := created
	sameRecipe.Mode = RunnerModeOnTheFly
	updated, err := s.UpdateRunner(ctx, sameRecipe)
	if err != nil {
		t.Fatalf("update runner mode: %v", err)
	}
	if updated.ContentHash != created.ContentHash {
		t.Errorf("mode change moved the hash: %q -> %q", created.ContentHash, updated.ContentHash)
	}

	changed := updated
	changed.Packages = append(changed.Packages, "git")
	rehashed, err := s.UpdateRunner(ctx, changed)
	if err != nil {
		t.Fatalf("update runner packages: %v", err)
	}
	if rehashed.ContentHash == created.ContentHash {
		t.Errorf("package change left the hash at %q", rehashed.ContentHash)
	}
	if len(rehashed.Packages) != 3 || rehashed.Packages[0] != "git" {
		t.Errorf("packages = %v, want canonical [git rsync sshpass]", rehashed.Packages)
	}

	if err := s.DeleteRunner(ctx, "fedora-ee"); err != nil {
		t.Fatalf("delete runner: %v", err)
	}
	if _, err := s.RunnerByName(ctx, "fedora-ee"); !errors.Is(err, ErrNotFound) {
		t.Errorf("read after delete: %v, want ErrNotFound", err)
	}
	if err := s.DeleteRunner(ctx, "fedora-ee"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateRunner(ctx, runnerFixture()); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of an unknown runner: %v, want ErrNotFound", err)
	}
}

func TestRunnerDuplicateName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	if _, err := s.CreateRunner(ctx, runnerFixture()); err != nil {
		t.Fatalf("create runner: %v", err)
	}
	if _, err := s.CreateRunner(ctx, runnerFixture()); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second create: %v, want ErrDuplicate", err)
	}
}

// TestRunnerRejectsBothBuildModels proves the mutual exclusion is enforced by
// the schema, not only by the CLI flag guard.
func TestRunnerRejectsBothBuildModels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	r := runnerFixture()
	r.Containerfile = ".exocomp/Containerfile"
	if _, err := s.CreateRunner(ctx, r); err == nil {
		t.Fatal("stored a runner naming both a bootstrap and a Containerfile")
	}

	bad := runnerFixture()
	bad.Mode = "podman"
	if _, err := s.CreateRunner(ctx, bad); err == nil {
		t.Fatal("stored a runner with an unknown mode")
	}
}

// TestRunnerHashIsCanonical pins the §7.1 properties the cache would key on:
// package order and duplication do not matter, the bootstrap body does, and the
// caps do not (they bound a run, not an image).
func TestRunnerHashIsCanonical(t *testing.T) {
	t.Parallel()

	base := runnerFixture()
	shuffled := base
	shuffled.Packages = []string{"sshpass", "rsync", "rsync"}
	if RunnerHash(base) != RunnerHash(shuffled) {
		t.Error("package order/duplication changed the hash")
	}

	capped := base
	capped.MemoryMax = "8G"
	capped.CPUQuota = "400%"
	if RunnerHash(base) != RunnerHash(capped) {
		t.Error("a caps change moved the hash")
	}

	edited := base
	edited.Bootstrap = base.Bootstrap + "microdnf install -y git\n"
	if RunnerHash(base) == RunnerHash(edited) {
		t.Error("a bootstrap edit left the hash unchanged")
	}

	rebased := base
	rebased.Base = "registry.fedoraproject.org/fedora-minimal@sha256:" + "0"
	if RunnerHash(base) == RunnerHash(rebased) {
		t.Error("a base change left the hash unchanged")
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
