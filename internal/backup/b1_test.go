package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestoreRefusesDowngradeWhenMarkerStripped is finding B1 end to end. The
// git leg's AUTHENTICATED.mac marker travels in the same ref an attacker with
// git write controls. An attacker who holds the public recipients strips BOTH
// keyring.age and the marker and plants a snapshot of their own with a
// self-consistent, untagged parts manifest. With the marker and the keyring
// gone, the restore has nothing in the repo left to tell it the target ever
// authenticated, so it treats the forgery as a never-authenticated legacy
// target and warns-and-proceeds — the downgrade.
//
// The fix is an out-of-band record: the recovery kit marks the target as
// authenticating (Source.Authenticates), which the in-repo strip cannot reach.
// With it, the planted snapshot is refused.
func TestRestoreRefusesDowngradeWhenMarkerStripped(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	f := newFixture(t)

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "init", "--bare", "--quiet", bare)
	target := f.target(t, "offsite-git", KindGit, bare)
	if report, err := f.engine.Run(t.Context(), []Target{target}); err != nil || report.Err() != nil {
		t.Fatalf("git leg: %v / %v", err, report.Err())
	}

	// The attacker's rewrite: strip keyring.age, its generation, and the
	// AUTHENTICATED.mac marker, then plant a forged, untagged snapshot the
	// operator's identity can still decrypt (it is encrypted to the public
	// recipients, which the attacker knows).
	const attackerDB = "ATTACKER-CONTROLLED DATABASE"
	plantForgedGitSnapshot(t, bare, f, attackerDB)

	// Before the fix — the residual gap, documented: with no out-of-band
	// assertion the restore accepts the forgery, returning the attacker's DB.
	loose := filepath.Join(t.TempDir(), "restored-loose")
	var warnings []string
	if _, err := RestoreLatest(t.Context(), Source{
		Kind:         KindGit,
		URI:          bare,
		Name:         "offsite-git",
		IdentityFile: f.identity,
		Warn:         func(msg string) { warnings = append(warnings, msg) },
	}, loose); err != nil {
		t.Fatalf("planted snapshot without the kit assertion: unexpected error %v", err)
	}
	body, err := os.ReadFile(filepath.Join(loose, RestoredDBName))
	if err != nil {
		t.Fatalf("read restored db: %v", err)
	}
	if !strings.Contains(string(body), attackerDB) {
		t.Fatalf("planted snapshot was not the one restored: %q", body)
	}
	t.Logf("attack (no assertion): restored the attacker db, warnings=%v", warnings)

	// After the fix: the recovery kit's out-of-band record that this target
	// authenticates turns the silent downgrade into a refusal.
	dest := filepath.Join(t.TempDir(), "restored")
	_, err = RestoreLatest(t.Context(), Source{
		Kind:          KindGit,
		URI:           bare,
		Name:          "offsite-git",
		IdentityFile:  f.identity,
		Authenticates: true,
	}, dest)
	if !errors.Is(err, ErrManifestUnauthenticated) {
		t.Fatalf("restore of a stripped-marker forgery with the kit assertion = %v, want ErrManifestUnauthenticated", err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, RestoredDBName)); statErr == nil {
		t.Fatal("a refused restore still wrote a database")
	}
	t.Logf("downgrade refused: %v", err)

	// The escape hatch stays explicit: an operator certain of the snapshot can
	// still accept it, and it is loud when they do.
	warnings = nil
	if _, err := RestoreLatest(t.Context(), Source{
		Kind:                 KindGit,
		URI:                  bare,
		Name:                 "offsite-git",
		IdentityFile:         f.identity,
		Authenticates:        true,
		AllowUnauthenticated: true,
		Warn:                 func(msg string) { warnings = append(warnings, msg) },
	}, filepath.Join(t.TempDir(), "restored-allowed")); err != nil {
		t.Fatalf("restore with --allow-unauthenticated: %v", err)
	}
	loud := false
	for _, w := range warnings {
		if strings.Contains(w, "integrity unverified") || strings.Contains(w, "not authenticated") {
			loud = true
		}
	}
	if !loud {
		t.Fatalf("--allow-unauthenticated restored quietly: %v", warnings)
	}
}

// plantForgedGitSnapshot rewrites the backup ref the way an attacker with git
// write and the public recipients would: it removes keyring.age, its
// generation file, and the AUTHENTICATED.mac marker, and writes a fresh,
// untagged snapshot carrying attackerDB. The parts manifest is self-consistent
// (so integrity passes), but it carries no .mac and there is no keyring to
// derive one from — the exact state finding B1 is about.
func plantForgedGitSnapshot(t *testing.T, bare string, f *fixture, attackerDB string) {
	t.Helper()

	rewriteBackupRef(t, bare, func(clone string) {
		// Strip every in-repo authenticity control and the keyring itself.
		matches, err := filepath.Glob(filepath.Join(clone, "snapshot-*"))
		if err != nil {
			t.Fatalf("glob snapshots: %v", err)
		}
		for _, m := range matches {
			if err := os.Remove(m); err != nil {
				t.Fatalf("remove %s: %v", m, err)
			}
		}
		for _, name := range []string{KeyringName, KeyringVersionName, AuthMarkerName} {
			if err := os.Remove(filepath.Join(clone, name)); err != nil && !os.IsNotExist(err) {
				t.Fatalf("remove %s: %v", name, err)
			}
		}

		// Build the attacker's staging tree. install needs snapshot.db, a
		// keyring.age (public ciphertext the attacker copies), and a manifest.
		tree := t.TempDir()
		if err := os.WriteFile(filepath.Join(tree, SnapshotName), []byte(attackerDB), 0o600); err != nil {
			t.Fatalf("write forged snapshot: %v", err)
		}
		keyring, err := f.env.KeyringAge()
		if err != nil {
			t.Fatalf("KeyringAge: %v", err)
		}
		if err := os.WriteFile(filepath.Join(tree, KeyringName), keyring, 0o600); err != nil {
			t.Fatalf("write forged keyring: %v", err)
		}
		if _, err := WriteManifest(tree, Manifest{SchemaVersion: 4}); err != nil {
			t.Fatalf("WriteManifest: %v", err)
		}

		archive, ext, err := archiveDir(t.Context(), tree)
		if err != nil {
			t.Fatalf("archiveDir: %v", err)
		}
		recipients, err := ageRecipients(f.env.Recipients())
		if err != nil {
			t.Fatalf("ageRecipients: %v", err)
		}
		sealed, err := ageEncrypt(archive, recipients)
		if err != nil {
			t.Fatalf("ageEncrypt: %v", err)
		}

		snapshot := "snapshot-20991231T235959Z" + ext + ".age"
		parts, manifest := chunkSnapshot(snapshot, sealed)
		for name, part := range parts {
			if err := os.WriteFile(filepath.Join(clone, name), part, 0o644); err != nil {
				t.Fatalf("write forged part %s: %v", name, err)
			}
		}
		if _, err := writePartsManifest(clone, manifest); err != nil {
			t.Fatalf("writePartsManifest: %v", err)
		}
	})
}
