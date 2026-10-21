package executor_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
)

// TestIdentityAllowlistAcceptsKeyFilesOutsideTheDataDir: a key file outside the
// data dir is accepted, cleaned, de-duplicated and sorted.
func TestIdentityAllowlistAcceptsKeyFilesOutsideTheDataDir(t *testing.T) {
	t.Parallel()
	data, keys := t.TempDir(), t.TempDir()
	a := writeKey(t, filepath.Join(keys, "a"))
	b := writeKey(t, filepath.Join(keys, "b"))

	got, err := executor.NormalizeIdentityAllowlist(data, []string{b, a + "/.", b})
	if err != nil {
		t.Fatalf("allowlist refused: %v", err)
	}
	if !slices.Equal(got, []string{a, b}) {
		t.Fatalf("allowlist = %v, want [%s %s]", got, a, b)
	}
}

// TestIdentityAllowlistRefusesWhatWouldWidenTheGrant: a relative path, a
// directory, a missing file, and a key inside the data dir, named directly or
// through a symlink outside it, are each refused.
func TestIdentityAllowlistRefusesWhatWouldWidenTheGrant(t *testing.T) {
	t.Parallel()
	data, keys := t.TempDir(), t.TempDir()
	inside := writeKey(t, filepath.Join(data, "keys", "id"))
	link := filepath.Join(keys, "link")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"relative":            "keys/id",
		"directory":           keys,
		"missing":             filepath.Join(keys, "absent"),
		"inside the data dir": inside,
		"symlink into it":     link,
	} {
		if _, err := executor.NormalizeIdentityAllowlist(data, []string{path}); err == nil {
			t.Errorf("%s (%s) was allowed", name, path)
		}
	}
}

// TestIdentityAllowlistRefusesTheUnwrapSecrets: the systemd credentials
// directory, the host-key passphrase file and the transit seal token file each
// unwrap the DEK, so none may be listed even though all sit outside the data
// dir.
func TestIdentityAllowlistRefusesTheUnwrapSecrets(t *testing.T) {
	data, creds := t.TempDir(), t.TempDir()
	passphrase := writeKey(t, filepath.Join(t.TempDir(), "passphrase"))
	sealToken := writeKey(t, filepath.Join(t.TempDir(), "seal-token"))
	t.Setenv(secrets.EnvCredentialsDirectory, creds)
	t.Setenv(secrets.EnvHostKeyPassphraseFile, passphrase)
	t.Setenv(secrets.EnvSealTokenFile, sealToken)

	for _, path := range []string{writeKey(t, filepath.Join(creds, "host.key")), passphrase, sealToken} {
		if _, err := executor.NormalizeIdentityAllowlist(data, []string{path}); err == nil {
			t.Errorf("%s was allowed", path)
		}
	}
}

func writeKey(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-a-real-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
