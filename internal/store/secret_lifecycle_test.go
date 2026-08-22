package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
)

// pinAtMigrations applies every embedded migration up to and including target
// against a raw database, then stamps user_version, producing the on-disk shape
// a binary at that schema version would have left behind.
func pinAtMigrations(t *testing.T, path string, target int) {
	t.Helper()
	ctx := context.Background()

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	for _, m := range migrations {
		if m.version > target {
			break
		}
		if _, err := raw.ExecContext(ctx, m.body); err != nil {
			t.Fatalf("apply migration %s: %v", m.name, err)
		}
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(target)); err != nil {
		t.Fatalf("set user_version to %d: %v", target, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
}

// TestMigration0017PreservesPreMigrationSecrets pins a database at user_version
// 16, seals a secret into it under the pre-0017 schema, then opens it with the
// current binary. The migration must apply cleanly, the pre-existing row must
// default to source='user' with no expiry, and the ciphertext sealed before the
// migration must still decrypt byte-for-byte — proof the AAD stayed 'name|kind'.
func TestMigration0017PreservesPreMigrationSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dataDir := t.TempDir()
	env, _, err := secrets.Init(dataDir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	defer func() { _ = env.Close() }()

	const name, kind, plaintext = "legacy_pass", "vault_pass", "the-old-value"
	sealed, err := env.Seal(name, kind, []byte(plaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	dbPath := filepath.Join(dataDir, "exocomp.db")
	pinAtMigrations(t, dbPath, 16)

	// Insert the sealed row through the pre-0017 schema (no source/expires_at).
	raw, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO secret (name, kind, nonce, ciphertext, aad, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		name, kind, sealed.Nonce, sealed.Ciphertext, name+"|"+kind, int64(1)); err != nil {
		t.Fatalf("insert pre-migration secret: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open at v16 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 17 {
		t.Fatalf("schema version = %d after open, want >= 17 (0017 applied)", version)
	}

	got, err := s.GetSecret(ctx, name)
	if err != nil {
		t.Fatalf("get migrated secret: %v", err)
	}
	if got.Source != SecretSourceUser {
		t.Errorf("migrated secret source = %q, want %q", got.Source, SecretSourceUser)
	}
	if got.ExpiresAt != nil {
		t.Errorf("migrated secret expires_at = %v, want nil", got.ExpiresAt)
	}

	// The ciphertext sealed before the migration must still open under the
	// unchanged name|kind AAD.
	pt, err := env.OpenSecret(secrets.SealedSecret{
		Name: got.Name, Kind: got.Kind, Nonce: got.Nonce, Ciphertext: got.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open pre-migration secret after 0017: %v", err)
	}
	if string(pt) != plaintext {
		t.Errorf("decrypted %q, want %q", pt, plaintext)
	}

	// A brand-new row created through the migrated schema also defaults to user.
	fresh, err := env.Seal("fresh", "generic", []byte("v"))
	if err != nil {
		t.Fatalf("seal fresh: %v", err)
	}
	if _, err := s.CreateSecret(ctx, SecretRow{
		Name: fresh.Name, Kind: fresh.Kind, Nonce: fresh.Nonce,
		Ciphertext: fresh.Ciphertext, AAD: "fresh|generic",
	}); err != nil {
		t.Fatalf("create fresh secret: %v", err)
	}
	freshRow, err := s.GetSecret(ctx, "fresh")
	if err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if freshRow.Source != SecretSourceUser {
		t.Errorf("fresh secret source = %q, want %q", freshRow.Source, SecretSourceUser)
	}
}
