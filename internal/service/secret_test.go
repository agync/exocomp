package service

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// plainSealer seals a value as itself, so a test can read back which value a
// row holds.
type plainSealer struct{}

func (plainSealer) Seal(name, kind string, pt []byte) (secrets.SealedSecret, error) {
	return secrets.SealedSecret{Name: name, Kind: kind, Nonce: []byte("n"), Ciphertext: pt}, nil
}

// A replace that also sets the expiry lands both or neither: when the expiry
// write fails, the old value is still the sealed one, so the error the caller
// sees is the whole truth.
func TestReplaceSecretWithExpiryIsAtomic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exocomp.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateSecret(ctx, store.SecretRow{
		Name: "k", Kind: "generic", Nonce: []byte("n"), Ciphertext: []byte("old"), Source: store.SecretSourceUser,
	}); err != nil {
		t.Fatal(err)
	}

	// A trigger stands in for an expiry write the database refuses.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER refuse_expiry BEFORE UPDATE OF expires_at ON secret
		BEGIN SELECT RAISE(ABORT, 'expiry refused'); END`); err != nil {
		t.Fatal(err)
	}

	at := time.Now().Add(time.Hour).UTC()
	_, err = New(st, t.TempDir(), nil).ReplaceSecret(ctx, plainSealer{}, ReplaceSecretInput{
		Name: "k", Value: []byte("new"), SetExpiry: true, ExpiresAt: &at,
	})
	if err == nil {
		t.Fatal("replace reported success although the expiry write failed")
	}
	got, err := st.GetSecret(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Ciphertext, []byte("old")) {
		t.Fatalf("sealed value = %q after a failed replace, want the old value kept", got.Ciphertext)
	}
}
