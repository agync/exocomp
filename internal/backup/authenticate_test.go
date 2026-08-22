package backup

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
)

// TestManifestMACKeyMatchesEnvelope pins the no-envelope derivation against the
// envelope's own, the same way TestResticPasswordMatchesEnvelope does: a
// restore host has the printed key and keyring.age, not an envelope.
func TestManifestMACKeyMatchesEnvelope(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	keyring, err := f.env.KeyringAge()
	if err != nil {
		t.Fatalf("KeyringAge: %v", err)
	}
	ids, err := ReadIdentities(f.identity)
	if err != nil {
		t.Fatalf("ReadIdentities: %v", err)
	}
	dek, err := UnwrapDEK(keyring, ids)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	defer clear(dek)

	for _, target := range []string{"nas", "offsite-git"} {
		got, err := ManifestMACKey(dek, target)
		if err != nil {
			t.Fatalf("ManifestMACKey(%q): %v", target, err)
		}
		if want := f.env.Derive(ManifestMACPurpose, target); !bytes.Equal(got, want) {
			t.Errorf("ManifestMACKey(%q) = %s, envelope derives %s",
				target, hex.EncodeToString(got), hex.EncodeToString(want))
		}
	}
}

// TestManifestMACPurposeIsOnTheCard keeps the purpose the backup legs use and
// the purpose table the recovery card prints from drifting apart.
func TestManifestMACPurposeIsOnTheCard(t *testing.T) {
	t.Parallel()

	if !slices.ContainsFunc(secrets.Purposes(), func(p secrets.Purpose) bool {
		return p.Name == ManifestMACPurpose
	}) {
		t.Fatalf("%q is not on the recovery card's purpose table", ManifestMACPurpose)
	}
}

// TestTamperingIsNotCorruption is the distinction the error messages have to
// keep: an edited manifest is a forgery, a rotted file is corruption.
func TestTamperingIsNotCorruption(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SnapshotName), []byte("database bytes"), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	if _, err := WriteManifest(dir, Manifest{SchemaVersion: 4}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	key := bytes.Repeat([]byte{0x5a}, dekLen)
	if err := AuthenticateManifest(dir, key, "nas"); err != nil {
		t.Fatalf("AuthenticateManifest: %v", err)
	}

	var warnings []string
	warn := func(msg string) { warnings = append(warnings, msg) }
	if err := authenticateSnapshotKey(dir, key, "nas", warn); err != nil {
		t.Fatalf("an untouched snapshot did not authenticate: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("an authenticated snapshot warned: %v", warnings)
	}

	// A rotted file: the manifest still authenticates, the digest does not.
	if err := os.WriteFile(filepath.Join(dir, SnapshotName), []byte("database bytez"), 0o600); err != nil {
		t.Fatalf("rewrite snapshot: %v", err)
	}
	manifest, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if err := authenticateSnapshotKey(dir, key, "nas", warn); err != nil {
		t.Fatalf("a corrupt file broke manifest authentication: %v", err)
	}
	err = manifest.Verify(dir)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("verify of a rotted file = %v, want a corruption error", err)
	}
	if errors.Is(err, ErrManifestTampered) {
		t.Error("a rotted file was reported as tampering")
	}

	// A rewritten manifest: the digests inside it are self-consistent, and it
	// still fails, because it is not the manifest this envelope wrote.
	body, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if _, err := WriteManifest(dir, Manifest{SchemaVersion: 4}); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	forged, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if err := forged.Verify(dir); err != nil {
		t.Fatalf("the forged manifest does not verify against its own files: %v", err)
	}
	err = authenticateSnapshotKey(dir, key, "nas", warn)
	if !errors.Is(err, ErrManifestTampered) {
		t.Fatalf("authenticate of a rewritten manifest = %v, want ErrManifestTampered", err)
	}
	if strings.Contains(err.Error(), "corrupt") {
		t.Errorf("tampering reported as corruption: %v", err)
	}
	t.Logf("tampered manifest refused: %v", err)

	// The same manifest under another target's subkey is equally not ours.
	if err := os.WriteFile(filepath.Join(dir, ManifestName), body, 0o600); err != nil {
		t.Fatalf("restore manifest: %v", err)
	}
	if err := authenticateSnapshotKey(dir, key, "offsite", warn); !errors.Is(err, ErrManifestTampered) {
		t.Fatalf("authenticate under the wrong target = %v, want ErrManifestTampered", err)
	}
}

// TestRestoreRefusesATamperedPartsManifest is the git leg's version of the
// same guard, end to end: the parts manifest is the one file a chunked
// snapshot exposes outside the age envelope, so whoever can write to the
// remote can rewrite it. The rewrite is refused as tampering, before the part
// digests it lists are trusted for anything.
func TestRestoreRefusesATamperedPartsManifest(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	f := newFixture(t)

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "init", "--bare", "--quiet", bare)
	target := f.target(t, "offsite-git", KindGit, bare)
	report, err := f.engine.Run(t.Context(), []Target{target})
	if err != nil || report.Err() != nil {
		t.Fatalf("git leg: %v / %v", err, report.Err())
	}

	rewriteBackupRef(t, bare, func(clone string) {
		name := partsManifestFile(t, clone)
		body, err := os.ReadFile(filepath.Join(clone, name))
		if err != nil {
			t.Fatalf("read parts manifest: %v", err)
		}
		// A digest an attacker would have to change to swap a part in.
		edited := bytes.Replace(body, []byte(`"sha256": "`), []byte(`"sha256": "0`), 1)
		if bytes.Equal(edited, body) {
			t.Fatal("parts manifest holds no sha256 to edit")
		}
		if err := os.WriteFile(filepath.Join(clone, name), edited, 0o644); err != nil {
			t.Fatalf("write parts manifest: %v", err)
		}
	})

	_, err = RestoreLatest(t.Context(), Source{
		Kind:         KindGit,
		URI:          bare,
		IdentityFile: f.identity,
	}, filepath.Join(t.TempDir(), "restored"))
	if !errors.Is(err, ErrManifestTampered) {
		t.Fatalf("restore of a rewritten parts manifest = %v, want ErrManifestTampered", err)
	}
	t.Logf("tampered parts manifest refused: %v", err)
}

// TestRestoreWarnsOnALegacySnapshot proves the compatibility half: a snapshot
// written before tags existed still restores, and says so loudly.
func TestRestoreWarnsOnALegacySnapshot(t *testing.T) {
	t.Parallel()
	requireTool(t, "git")
	f := newFixture(t)

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "init", "--bare", "--quiet", bare)
	target := f.target(t, "offsite-git", KindGit, bare)
	if report, err := f.engine.Run(t.Context(), []Target{target}); err != nil || report.Err() != nil {
		t.Fatalf("git leg: %v / %v", err, report.Err())
	}

	rewriteBackupRef(t, bare, func(clone string) {
		gitRun(t, clone, "rm", "--quiet", partsManifestFile(t, clone)+macSuffix)
	})

	var warnings []string
	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreLatest(t.Context(), Source{
		Kind:         KindGit,
		URI:          bare,
		IdentityFile: f.identity,
		Warn:         func(msg string) { warnings = append(warnings, msg) },
	}, dest); err != nil {
		t.Fatalf("restore of an untagged snapshot: %v", err)
	}

	var loud bool
	for _, w := range warnings {
		if strings.Contains(w, "legacy backup: integrity unverified") {
			loud = true
			t.Logf("warned: %s", w)
		}
	}
	if !loud {
		t.Fatalf("an untagged snapshot restored quietly: %v", warnings)
	}
	assertRestoredChain(t, f, dest, "offsite-git")
}

// partsManifestFile names the one parts manifest in a clone of the backup ref.
func partsManifestFile(t *testing.T, clone string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(clone, "snapshot-*"+partsSuffix))
	if err != nil || len(matches) != 1 {
		t.Fatalf("parts manifests in %s: %v (%v)", clone, matches, err)
	}
	return filepath.Base(matches[0])
}

// rewriteBackupRef clones the bare repository, lets edit change the tree, and
// force-pushes the result back onto the backup ref — what someone who can
// write to the git remote can do.
func rewriteBackupRef(t *testing.T, bare string, edit func(clone string)) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "rewritten")
	gitRun(t, "", "clone", "--quiet", "--branch", "backup", bare, clone)
	edit(clone)
	gitRun(t, clone, "add", "--all")
	gitRun(t, clone, "-c", "user.name=rewriter", "-c", "user.email=rewriter@invalid",
		"commit", "-q", "--no-gpg-sign", "-m", "rewritten")
	gitRun(t, clone, "push", "--force", "--quiet", bare, "HEAD:"+backupRef)
}

// TestUnauthenticatedRestoreWarns covers the two ways a restore ends up with no
// tag to check: an old backup, and a restore run from a bare repository
// password with no keyring at hand.
func TestUnauthenticatedRestoreWarns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, SnapshotName), []byte("database bytes"), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	if _, err := WriteManifest(dir, Manifest{SchemaVersion: 4}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	var warnings []string
	warn := func(msg string) { warnings = append(warnings, msg) }

	key := bytes.Repeat([]byte{0x5a}, dekLen)
	if err := authenticateSnapshotKey(dir, key, "nas", warn); err != nil {
		t.Fatalf("a legacy snapshot refused to restore: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "legacy backup: integrity unverified") {
		t.Fatalf("legacy warnings = %v", warnings)
	}

	warnings = nil
	if err := authenticateSnapshot(dir, nil, "nas", warn); err != nil {
		t.Fatalf("a keyring-less restore refused: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not authenticated") {
		t.Fatalf("keyring-less warnings = %v", warnings)
	}
}
