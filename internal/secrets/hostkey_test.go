package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

// evilMaidRecoversDEK is the E1 attack: given only a copy of the data dir, read
// host.key, unwrap keyring.age with it, and recover the DEK. It returns true
// when the attack succeeds — i.e. the data dir alone yields the master key.
func evilMaidRecoversDEK(t *testing.T, stolenDir string, wantDEK []byte) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stolenDir, hostKeyFile))
	if err != nil {
		return false // no plaintext host.key to read
	}
	id, err := age.ParseX25519Identity(string(bytes.TrimSpace(raw)))
	if err != nil {
		return false
	}
	keyring, err := os.ReadFile(filepath.Join(stolenDir, keyringFile))
	if err != nil {
		return false
	}
	dek, err := unwrapDEK(keyring, id)
	if err != nil {
		return false
	}
	return bytes.Equal(dek, wantDEK)
}

// TestPassphraseWrappedHostKeyDefeatsEvilMaid is the E1 regression test. With
// an operator passphrase set, Init must leave no plaintext host.key in the data
// dir, so a stolen data volume does not contain the key that unwraps the DEK.
// Before the fix Init always wrote a plaintext host.key and the attack works.
func TestPassphraseWrappedHostKeyDefeatsEvilMaid(t *testing.T) {
	t.Setenv(EnvHostKeyPassphrase, "correct horse battery staple")
	dir := t.TempDir()
	env, _ := mustInit(t, dir, nil)

	if _, err := os.Stat(filepath.Join(dir, hostKeyFile)); err == nil {
		t.Fatal("Init left a plaintext host.key despite a passphrase being set")
	}
	if _, err := os.Stat(filepath.Join(dir, hostKeyAgeFile)); err != nil {
		t.Fatalf("Init did not write the passphrase-wrapped host key: %v", err)
	}
	if got := InspectHostKeyProtection(dir); got != HostKeyPassphrase {
		t.Errorf("protection = %v, want passphrase", got)
	}

	// The evil maid copies the whole data dir and gets nothing usable.
	stolen := t.TempDir()
	copyDir(t, dir, stolen)
	if evilMaidRecoversDEK(t, stolen, env.dek) {
		t.Fatal("evil maid recovered the DEK from the stolen data dir alone")
	}

	// The legitimate daemon, holding the passphrase, still opens.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with passphrase: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if !bytes.Equal(reopened.dek, env.dek) {
		t.Error("Open recovered a different DEK")
	}
	if reopened.HostKeyProtection() != HostKeyPassphrase {
		t.Errorf("opened protection = %v, want passphrase", reopened.HostKeyProtection())
	}
}

// TestSystemdCredentialHostKey proves the daemon reads host.key from the
// systemd credential dir when present, leaving the data volume without a
// plaintext key.
func TestSystemdCredentialHostKey(t *testing.T) {
	dir := t.TempDir()
	env, _ := mustInit(t, dir, nil) // plaintext host.key in dir for this setup

	credDir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(dir, hostKeyFile))
	if err != nil {
		t.Fatalf("read host key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credDir, hostKeyFile), raw, 0o400); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	// The data volume carries no plaintext key: only the credential dir does.
	if err := os.Remove(filepath.Join(dir, hostKeyFile)); err != nil {
		t.Fatalf("remove plaintext host key: %v", err)
	}
	t.Setenv(EnvCredentialsDirectory, credDir)

	if got := InspectHostKeyProtection(dir); got != HostKeySystemdCredential {
		t.Errorf("protection = %v, want systemd-credential", got)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open from credential dir: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if !bytes.Equal(reopened.dek, env.dek) {
		t.Error("Open from credential recovered a different DEK")
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
	}
}
