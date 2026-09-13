package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// runRestore executes the restore command with args and returns stdout, stderr,
// and the error, so a test can assert on the honest refusal without the process
// exiting.
func runRestore(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newRestoreCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestLitestreamRestore_RequiresIdentity: the decrypt round-trip is the gate that
// makes a Litestream restore trustworthy, so the identity that drives it is not
// optional.
func TestLitestreamRestore_RequiresIdentity(t *testing.T) {
	_, _, err := runRestore(t, "--from-litestream", "--dest", t.TempDir(), "--replica", "file:///srv/r")
	if err == nil || !strings.Contains(err.Error(), "--identity") {
		t.Fatalf("err = %v, want an --identity refusal", err)
	}
}

// TestLitestreamRestore_RefusesWithoutKeyring: Litestream recovers the database
// only; onto a dir with no keyring there is nothing to open it with, so the
// restore refuses and points at the authoritative restic restore.
func TestLitestreamRestore_RefusesWithoutKeyring(t *testing.T) {
	dest := t.TempDir()
	identity := writeThrowawayIdentity(t)

	_, _, err := runRestore(t, "--from-litestream", "--dest", dest,
		"--identity", identity, "--replica", "file:///srv/r")
	if err == nil || !strings.Contains(err.Error(), backup.KeyringName) {
		t.Fatalf("err = %v, want a missing-keyring refusal naming %s", err, backup.KeyringName)
	}
	if !strings.Contains(err.Error(), "restic") {
		t.Fatalf("the refusal should point at the restic restore; got %v", err)
	}
}

// TestLitestreamRestore_RefusesWithoutReplica: with no --replica and no stored
// setting, there is no target to restore from — an honest refusal, not a guess.
func TestLitestreamRestore_RefusesWithoutReplica(t *testing.T) {
	useDataDir(t, t.TempDir())
	dest := sealedDestDir(t)
	identity := writeThrowawayIdentity(t)

	_, _, err := runRestore(t, "--from-litestream", "--dest", dest, "--identity", identity)
	if err == nil || !strings.Contains(err.Error(), backup.SettingReplicaURL) {
		t.Fatalf("err = %v, want a no-replica refusal naming %s", err, backup.SettingReplicaURL)
	}
}

// TestLitestreamRestore_RefusesWhenBinaryAbsent: with a keyring and a replica in
// hand, the restore still refuses cleanly when the sidecar binary is not
// installed, rather than shelling a missing tool. When litestream IS installed
// the refusal cannot be exercised, so the test skips.
func TestLitestreamRestore_RefusesWhenBinaryAbsent(t *testing.T) {
	if backup.LitestreamAvailable() {
		t.Skip("litestream is installed here; the binary-absent refusal cannot be exercised")
	}
	dest := sealedDestDir(t)
	identity := writeThrowawayIdentity(t)

	_, _, err := runRestore(t, "--from-litestream", "--dest", dest,
		"--identity", identity, "--replica", "file://"+t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "litestream") {
		t.Fatalf("err = %v, want a litestream-absent refusal", err)
	}
}

// TestLitestreamRestore_RefusesUnsafeReplica: a plaintext public remote is
// refused before any subprocess unless the operator acknowledges the store is
// encrypted — Litestream ships plaintext WAL frames. This holds whether or not
// the binary is installed (a box without it refuses on the binary first; either
// way it is a refusal, never a run).
func TestLitestreamRestore_RefusesUnsafeReplica(t *testing.T) {
	dest := sealedDestDir(t)
	identity := writeThrowawayIdentity(t)

	_, _, err := runRestore(t, "--from-litestream", "--dest", dest,
		"--identity", identity, "--replica", "s3://s3.amazonaws.com/bucket/exocomp")
	if err == nil {
		t.Fatal("a plaintext public-remote replica was accepted without acknowledgement")
	}
	if !strings.Contains(err.Error(), "acknowledge") && !strings.Contains(err.Error(), "litestream") {
		t.Fatalf("err = %v, want an unsafe-replica or binary-absent refusal", err)
	}
}

// TestLitestreamRestore_RefusesSnapshotSelector: --snapshot selects a restic
// snapshot, but the replica leg restores the database only and returns before
// the restic source is parsed, so combining them would silently drop the
// selector — the refusal the round exists to kill.
func TestLitestreamRestore_RefusesSnapshotSelector(t *testing.T) {
	_, _, err := runRestore(t, "--from-litestream", "--dest", t.TempDir(),
		"--replica", "file:///srv/r", "--snapshot", "a3f9c2abcdef")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with --from-litestream") {
		t.Fatalf("err = %v, want a --snapshot + --from-litestream refusal", err)
	}
}

// sealedDestDir builds a destination data dir holding keyring.age — the shape a
// prior restic restore leaves — so a Litestream restore has something to open the
// recovered database with.
func sealedDestDir(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "restored")
	env, _, err := secrets.Init(dest, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })
	if _, err := os.Stat(filepath.Join(dest, backup.KeyringName)); err != nil {
		t.Fatalf("secrets.Init left no keyring: %v", err)
	}
	return dest
}

// writeThrowawayIdentity writes a valid age recovery identity that is unrelated
// to any keyring, for tests that refuse before the decrypt round-trip runs.
func writeThrowawayIdentity(t *testing.T) string {
	t.Helper()
	_, kit, err := secrets.Init(filepath.Join(t.TempDir(), "id"), nil)
	if err != nil {
		t.Fatalf("secrets.Init(id): %v", err)
	}
	path := filepath.Join(t.TempDir(), "recovery.key")
	if err := os.WriteFile(path, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	return path
}

// TestLitestreamRestore_E2E is the round-trip that shells the real litestream
// binary: seed a replica from a live database, recover it with `restore`, and
// prove the decrypt round-trip. It SKIPs cleanly when litestream is not
// installed — like the tofu/ansible gates — so the deterministic units above
// carry the logic and this proves the shell path only where the tool exists.
func TestLitestreamRestore_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if !backup.LitestreamAvailable() {
		t.Skip("litestream binary absent; the shell round-trip e2e is skipped (config-gen + decision logic are unit-tested without it)")
	}

	// A live data dir with a real, sealed database.
	dataDir := filepath.Join(t.TempDir(), "live")
	env, kit, err := secrets.Init(dataDir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })
	identity := filepath.Join(t.TempDir(), "recovery.key")
	if err := os.WriteFile(identity, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	dbPath := filepath.Join(dataDir, backup.RestoredDBName)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	sealed, err := env.Seal("canary", "canary", []byte("e2e-proof"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := st.CreateSecret(t.Context(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce,
		Ciphertext: sealed.Ciphertext, AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		t.Fatalf("create sealed row: %v", err)
	}
	_ = st.Close()

	// Seed a file replica by running the replicate daemon briefly, then stopping
	// it once the replica has generations to restore from.
	replicaDir := filepath.Join(t.TempDir(), "replica")
	replicaURL := "file://" + replicaDir
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	rep := exec.CommandContext(ctx, "litestream", "replicate", dbPath, replicaURL)
	if err := rep.Start(); err != nil {
		t.Fatalf("start litestream replicate: %v", err)
	}
	waitForReplica(t, replicaDir)
	_ = rep.Process.Kill()
	_ = rep.Wait()

	// Recover into a fresh dir that already holds the keyring (as a restic restore
	// would have left it), then prove the decrypt round-trip.
	dest := sealedDestDir(t)
	if err := backup.LitestreamRestore(t.Context(), backup.LitestreamConfig{
		ReplicaURL: replicaURL,
		DestDB:     filepath.Join(dest, backup.RestoredDBName),
	}); err != nil {
		t.Fatalf("LitestreamRestore: %v", err)
	}
	name, err := backup.VerifyEnvelopeOpens(t.Context(), dest, identity)
	if err != nil {
		t.Fatalf("decrypt round-trip: %v", err)
	}
	if name != "canary" {
		t.Fatalf("opened %q, want the sealed canary row", name)
	}
}

// waitForReplica blocks until the file replica holds a generation directory, so
// the restore that follows has something to read.
func waitForReplica(t *testing.T, replicaDir string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		generations := filepath.Join(replicaDir, "generations")
		if entries, err := os.ReadDir(generations); err == nil && len(entries) > 0 {
			time.Sleep(500 * time.Millisecond) // let the first WAL segment land
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("litestream produced no replica generations within the deadline")
}
