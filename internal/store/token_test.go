package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

func openTokenStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestTokenLifecycle(t *testing.T) {
	st := openTokenStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := st.CreateToken(ctx, store.APIToken{
		Name: "laptop", Scope: store.ScopeOperator, Prefix: "abc123", Hash: "$argon2id$stub",
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("created token has no id")
	}
	if !created.Active(now) {
		t.Fatal("a fresh token with no expiry must be active")
	}

	// A count of active tokens is what a non-loopback bind checks.
	n, err := st.CountActiveTokens(ctx, now)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("active count = %d, want 1", n)
	}

	// Revocation is immediate and drops the active count to zero.
	if err := st.RevokeToken(ctx, "laptop"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	n, err = st.CountActiveTokens(ctx, now)
	if err != nil {
		t.Fatalf("count after revoke: %v", err)
	}
	if n != 0 {
		t.Fatalf("active count after revoke = %d, want 0", n)
	}
	got, err := st.GetToken(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after revoke: %v", err)
	}
	if got.Active(now) {
		t.Fatal("a revoked token must not be active")
	}

	// A second revoke changes nothing and says so.
	if err := st.RevokeToken(ctx, "laptop"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second revoke error = %v, want ErrNotFound", err)
	}
}

func TestTokenExpiryExcludedFromActive(t *testing.T) {
	st := openTokenStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-time.Minute)

	if _, err := st.CreateToken(ctx, store.APIToken{
		Name: "expired", Scope: store.ScopeReadonly, Prefix: "p", Hash: "h", ExpiresAt: &past,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := st.CountActiveTokens(ctx, now)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired token counted active: %d", n)
	}
}

func TestTokenDuplicateName(t *testing.T) {
	st := openTokenStore(t)
	ctx := context.Background()
	if _, err := st.CreateToken(ctx, store.APIToken{Name: "a", Scope: store.ScopeApprove, Hash: "h"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := st.CreateToken(ctx, store.APIToken{Name: "a", Scope: store.ScopeApprove, Hash: "h"})
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate name error = %v, want ErrDuplicate", err)
	}
}

func TestTokenScopeRejected(t *testing.T) {
	st := openTokenStore(t)
	if _, err := st.CreateToken(context.Background(), store.APIToken{Name: "x", Scope: "root", Hash: "h"}); err == nil {
		t.Fatal("an unknown scope must be refused")
	}
}
