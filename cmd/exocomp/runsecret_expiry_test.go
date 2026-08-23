package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// TestSecretExpired covers the pure predicate the inject-time warn keys on: a
// deadline in the past is expired, one in the future or absent is not.
func TestSecretExpired(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	if !secretExpired(store.SecretRow{ExpiresAt: &past}, now) {
		t.Error("a past deadline should be expired")
	}
	if secretExpired(store.SecretRow{ExpiresAt: &future}, now) {
		t.Error("a future deadline should not be expired")
	}
	if secretExpired(store.SecretRow{ExpiresAt: nil}, now) {
		t.Error("no deadline should never be expired")
	}
}

// TestResolveExpiredRunSecretWarnsButStillInjects is the item-2 guard: an
// expired sealed secret is warned about, naming the secret but never its value,
// and the run still gets the value — expiry warns, it never blocks or drops.
func TestResolveExpiredRunSecretWarnsButStillInjects(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	const tokenVal = "super-secret-token-value"
	sealSecretDirect(t, dir, "prox-token", tokenVal)
	if out, err := runRunSecret(t, "add", "prox", "PROXMOX_PASSWORD", "prox-token"); err != nil {
		t.Fatalf("run-secret add: %v (%s)", err, out)
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Backdate the secret's deadline so resolution sees it as expired.
	past := time.Now().UTC().Add(-24 * time.Hour)
	if err := st.SetSecretExpiry(ctx, "prox-token", &past); err != nil {
		t.Fatalf("set expiry: %v", err)
	}

	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()

	// Capture the structured log for the run of this resolution.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	rs, _, err := resolveRunSecrets(ctx, st, env, "prox", "")
	if err != nil {
		t.Fatalf("resolveRunSecrets refused an expired secret: %v", err)
	}

	// The run still gets the value: warn, never block.
	if rs["PROXMOX_PASSWORD"] != tokenVal {
		t.Fatalf("PROXMOX_PASSWORD = %q, want the sealed value despite expiry", rs["PROXMOX_PASSWORD"])
	}

	logged := buf.String()
	if !strings.Contains(logged, "expiry") && !strings.Contains(logged, "expired") {
		t.Fatalf("expected an expiry warning, got: %s", logged)
	}
	if !strings.Contains(logged, "prox-token") {
		t.Fatalf("warning should name the secret: %s", logged)
	}
	// The value must never reach a log (AGENTS.md invariant 4).
	if strings.Contains(logged, tokenVal) {
		t.Fatalf("secret value leaked into the warning: %s", logged)
	}
}
