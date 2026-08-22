package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
	"filippo.io/age"
)

// TestRestoreRefusesADowngradedKeyring is the rollback attack, end to end: the
// operator revokes a recipient, which bumps the keyring generation, and
// something puts the old keyring back at the target root. The old keyring still
// unwraps the DEK — that is the point of it — so nothing but the generation
// catches it.
func TestRestoreRefusesADowngradedKeyring(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	laptop, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	if _, err := secrets.Migrate(f.dataDir, secrets.MigrateOptions{
		Add: []string{"laptop=" + laptop.Recipient().String()},
	}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	env, err := secrets.Open(f.dataDir)
	if err != nil {
		t.Fatalf("reopen envelope: %v", err)
	}
	defer func() { _ = env.Close() }()
	if got := env.KeyringVersion().Version; got != 2 {
		t.Fatalf("migrated keyring is v%d, want v2", got)
	}

	repo := filepath.Join(t.TempDir(), "nas")
	target := f.target(t, "nas", KindResticLocal, repo)
	engine := New(f.store, env, Options{})
	report, err := engine.Run(t.Context(), []Target{target})
	if err != nil || report.Err() != nil {
		t.Fatalf("backup under v2: %v / %v", err, report.Err())
	}
	if report.Manifest.KeyringVersion != 2 {
		t.Fatalf("manifest records keyring v%d, want v2", report.Manifest.KeyringVersion)
	}

	root := localRepoPath(repo)
	current, err := os.ReadFile(filepath.Join(root, KeyringVersionName))
	if err != nil {
		t.Fatalf("read target root version: %v", err)
	}

	// The crafted rollback: the generation at the target root goes back to the
	// one before the recipient was added.
	rolled := secrets.MarshalKeyringVersion(secrets.KeyringVersion{Version: 1})
	if err := os.WriteFile(filepath.Join(root, KeyringVersionName), rolled, 0o644); err != nil {
		t.Fatalf("write rolled-back version: %v", err)
	}

	_, err = RestoreLatest(t.Context(), Source{
		Kind:         KindResticLocal,
		URI:          repo,
		IdentityFile: f.identity,
	}, filepath.Join(t.TempDir(), "restored"))
	if !errors.Is(err, ErrKeyringDowngrade) {
		t.Fatalf("restore over a rolled-back keyring = %v, want ErrKeyringDowngrade", err)
	}
	if !strings.Contains(err.Error(), "v1") || !strings.Contains(err.Error(), "v2") {
		t.Errorf("refusal does not name both generations: %v", err)
	}
	t.Logf("downgrade refused: %v", err)

	// The same restore succeeds once the current generation is back in place,
	// so the refusal is the version and not the target.
	if err := os.WriteFile(filepath.Join(root, KeyringVersionName), current, 0o644); err != nil {
		t.Fatalf("restore target root version: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "restored-ok")
	if _, err := RestoreLatest(t.Context(), Source{
		Kind:         KindResticLocal,
		URI:          repo,
		IdentityFile: f.identity,
	}, dest); err != nil {
		t.Fatalf("restore under the current keyring: %v", err)
	}
	restored, err := secrets.ReadKeyringVersion(dest)
	if err != nil {
		t.Fatalf("ReadKeyringVersion(dest): %v", err)
	}
	if restored.Version != 2 {
		t.Errorf("restored data dir is %s, want v2", restored)
	}
}

// TestCheckKeyringVersion covers the cases a real target cannot easily be put
// into: a snapshot whose own keyring was spliced back, and a manifest from
// before the counter existed.
func TestCheckKeyringVersion(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, version int) string {
		t.Helper()
		dir := t.TempDir()
		if version == 0 {
			return dir
		}
		body := secrets.MarshalKeyringVersion(secrets.KeyringVersion{Version: version})
		if err := os.WriteFile(filepath.Join(dir, KeyringVersionName), body, 0o600); err != nil {
			t.Fatalf("write version: %v", err)
		}
		return dir
	}

	cases := map[string]struct {
		snapshot int
		root     int
		manifest int
		refused  bool
	}{
		"current everywhere":     {snapshot: 3, root: 3, manifest: 3, refused: false},
		"root ahead":             {snapshot: 3, root: 4, manifest: 3, refused: false},
		"root rolled back":       {snapshot: 3, root: 2, manifest: 3, refused: true},
		"snapshot spliced back":  {snapshot: 1, root: 3, manifest: 3, refused: true},
		"snapshot has no file":   {snapshot: 0, root: 3, manifest: 3, refused: true},
		"root unknown":           {snapshot: 3, root: 0, manifest: 3, refused: false},
		"manifest predates it":   {snapshot: 1, root: 1, manifest: 0, refused: false},
		"first generation only":  {snapshot: 1, root: 1, manifest: 1, refused: false},
		"both rolled back to v1": {snapshot: 1, root: 1, manifest: 4, refused: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := checkKeyringVersion(write(t, tc.snapshot), tc.root, Manifest{KeyringVersion: tc.manifest})
			if tc.refused && !errors.Is(err, ErrKeyringDowngrade) {
				t.Fatalf("checkKeyringVersion = %v, want ErrKeyringDowngrade", err)
			}
			if !tc.refused && err != nil {
				t.Fatalf("checkKeyringVersion = %v, want nil", err)
			}
		})
	}
}
