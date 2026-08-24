package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

func TestRotateTokenReseals(t *testing.T) {
	st := openTokenStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := st.CreateToken(ctx, store.APIToken{
		Name: "laptop", Scope: store.ScopeOperator, Prefix: "old123", Hash: "$argon2id$old",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.CredentialEpoch() != created.CreatedAt.UnixMilli() {
		t.Fatal("a fresh token's credential epoch is its creation time")
	}

	rotateAt := now.Add(time.Second)
	rotated, err := st.RotateToken(ctx, "laptop", "$argon2id$new", "new456", nil, false, rotateAt)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// Identity is preserved; only the secret material and rotation stamp change.
	if rotated.ID != created.ID || rotated.Name != created.Name || rotated.Scope != created.Scope {
		t.Fatalf("rotate changed identity: %+v vs %+v", rotated, created)
	}
	if rotated.Hash != "$argon2id$new" || rotated.Prefix != "new456" {
		t.Fatalf("rotate did not reseal: hash=%q prefix=%q", rotated.Hash, rotated.Prefix)
	}
	if rotated.RotatedAt == nil {
		t.Fatal("rotate did not stamp rotated_at")
	}
	if rotated.CredentialEpoch() != rotated.RotatedAt.UnixMilli() {
		t.Fatal("a rotated token's credential epoch is its rotation time")
	}
	if rotated.CredentialEpoch() == created.CredentialEpoch() {
		t.Fatal("rotate must advance the credential epoch (else sessions survive)")
	}
	if !rotated.Active(rotateAt) {
		t.Fatal("a rotated token must be active")
	}

	// The name is still reserved: it cannot be re-minted even after rotation.
	if _, err := st.CreateToken(ctx, store.APIToken{
		Name: "laptop", Scope: store.ScopeReadonly, Prefix: "x", Hash: "$argon2id$x",
	}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("re-mint of a rotated name = %v, want ErrDuplicate", err)
	}
}

func TestRotateTokenRevivesRevoked(t *testing.T) {
	st := openTokenStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := st.CreateToken(ctx, store.APIToken{
		Name: "abn", Scope: store.ScopeOperator, Prefix: "p", Hash: "$argon2id$a",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.RevokeToken(ctx, "abn"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	rotated, err := st.RotateToken(ctx, "abn", "$argon2id$b", "q", nil, false, now)
	if err != nil {
		t.Fatalf("rotate revoked: %v", err)
	}
	if rotated.RevokedAt != nil {
		t.Fatal("rotating a revoked token must clear its revocation")
	}
	if !rotated.Active(now) {
		t.Fatal("a rotated (revived) token must be active")
	}
}

func TestRotateTokenExpiry(t *testing.T) {
	st := openTokenStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	exp := now.Add(24 * time.Hour)

	if _, err := st.CreateToken(ctx, store.APIToken{
		Name: "ci", Scope: store.ScopeOperator, Prefix: "p", Hash: "$argon2id$a", ExpiresAt: &exp,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// setExpiry=false keeps the existing expiry.
	kept, err := st.RotateToken(ctx, "ci", "$argon2id$b", "q", nil, false, now)
	if err != nil {
		t.Fatalf("rotate keep: %v", err)
	}
	if kept.ExpiresAt == nil || kept.ExpiresAt.UnixMilli() != exp.UnixMilli() {
		t.Fatalf("rotate without setExpiry changed expiry: %v", kept.ExpiresAt)
	}

	// setExpiry=true with nil clears the expiry.
	cleared, err := st.RotateToken(ctx, "ci", "$argon2id$c", "r", nil, true, now)
	if err != nil {
		t.Fatalf("rotate clear: %v", err)
	}
	if cleared.ExpiresAt != nil {
		t.Fatalf("rotate with setExpiry+nil must clear expiry, got %v", cleared.ExpiresAt)
	}
}

func TestRotateTokenMissing(t *testing.T) {
	st := openTokenStore(t)
	if _, err := st.RotateToken(context.Background(), "ghost", "$argon2id$x", "p", nil, false, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rotate missing = %v, want ErrNotFound", err)
	}
}
