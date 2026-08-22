package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// runSecrets executes the secrets command tree with the given stdin and args.
func runSecrets(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newSecretsCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// openSecretValue reads a sealed secret back through the envelope, returning its
// plaintext and row. Used only in tests to prove what was sealed.
func openSecretValue(t *testing.T, dir, name string) (string, store.SecretRow) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	row, err := st.GetSecret(context.Background(), name)
	if err != nil {
		t.Fatalf("get secret %q: %v", name, err)
	}
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()
	pt, err := env.OpenSecret(secrets.SealedSecret{
		Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open secret %q: %v", name, err)
	}
	return string(pt), row
}

func TestSecretsAddGenerateDecryptable(t *testing.T) {
	dir := initForSource(t)

	out, err := runSecrets(t, "", "add", "api-key", "--generate")
	if err != nil {
		t.Fatalf("add --generate: %v (%s)", err, out)
	}
	if !strings.Contains(out, "source generated") {
		t.Errorf("add output = %q, want it to name the generated source", out)
	}

	val, row := openSecretValue(t, dir, "api-key")
	if row.Source != store.SecretSourceGenerated {
		t.Errorf("source = %q, want generated", row.Source)
	}
	if len(val) == 0 {
		t.Error("generated secret decrypted to empty")
	}
}

func TestSecretsAddUserWithExpiry(t *testing.T) {
	dir := initForSource(t)

	out, err := runSecrets(t, "hunter2\n", "add", "db-pass", "--expires", "720h")
	if err != nil {
		t.Fatalf("add user: %v (%s)", err, out)
	}
	if !strings.Contains(out, "source user") || !strings.Contains(out, "expires") {
		t.Errorf("add output = %q, want source user and an expiry", out)
	}

	val, row := openSecretValue(t, dir, "db-pass")
	if val != "hunter2" {
		t.Errorf("decrypted %q, want hunter2", val)
	}
	if row.Source != store.SecretSourceUser {
		t.Errorf("source = %q, want user", row.Source)
	}
	if row.ExpiresAt == nil {
		t.Fatal("expires_at is nil, want a future instant")
	}
	if !row.ExpiresAt.After(time.Now().UTC()) {
		t.Errorf("expires_at = %v, want in the future", row.ExpiresAt)
	}
}

func TestSecretsRotateGeneratedChangesValue(t *testing.T) {
	dir := initForSource(t)

	if out, err := runSecrets(t, "", "add", "token", "--generate"); err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}
	before, _ := openSecretValue(t, dir, "token")

	out, err := runSecrets(t, "", "rotate", "token")
	if err != nil {
		t.Fatalf("rotate: %v (%s)", err, out)
	}
	if !strings.Contains(out, "regenerated") {
		t.Errorf("rotate output = %q, want it to say regenerated", out)
	}
	after, row := openSecretValue(t, dir, "token")
	if after == before {
		t.Error("rotate did not change the generated value")
	}
	if len(after) == 0 {
		t.Error("rotated value decrypted to empty")
	}
	if row.RotatedAt == nil {
		t.Error("rotated_at not stamped")
	}
}

func TestSecretsRotateUserFromStdin(t *testing.T) {
	dir := initForSource(t)

	if out, err := runSecrets(t, "old-value\n", "add", "cred"); err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}
	before, _ := openSecretValue(t, dir, "cred")
	if before != "old-value" {
		t.Fatalf("initial value = %q, want old-value", before)
	}

	out, err := runSecrets(t, "new-value\n", "rotate", "cred")
	if err != nil {
		t.Fatalf("rotate: %v (%s)", err, out)
	}
	if !strings.Contains(out, "reminder:") {
		t.Errorf("rotate output = %q, want the upstream-revoke reminder", out)
	}
	after, _ := openSecretValue(t, dir, "cred")
	if after != "new-value" {
		t.Errorf("rotated value = %q, want new-value", after)
	}
}

func TestSecretsRotateUserEmptyStdinFails(t *testing.T) {
	initForSource(t)
	if out, err := runSecrets(t, "secret\n", "add", "cred"); err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}
	if _, err := runSecrets(t, "", "rotate", "cred"); err == nil {
		t.Error("rotate of a user secret with empty stdin should fail")
	}
}

func TestSecretsRmGuardsBoundSecret(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	sealSecretDirect(t, dir, "prox-token", "value")
	if out, err := runRunSecret(t, "add", "prox", "PROXMOX_PASSWORD", "prox-token"); err != nil {
		t.Fatalf("run-secret add: %v (%s)", err, out)
	}

	out, err := runSecrets(t, "", "rm", "prox-token")
	if err == nil {
		t.Fatalf("rm of a bound secret should fail; output = %q", out)
	}
	if !strings.Contains(out, "prox") || !strings.Contains(out, "PROXMOX_PASSWORD") {
		t.Errorf("refusal did not name the binding: %q", out)
	}

	// The secret still exists after the refusal.
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.GetSecret(ctx, "prox-token"); err != nil {
		t.Errorf("secret gone after a refused rm: %v", err)
	}
	_ = st.Close()

	out, err = runSecrets(t, "", "rm", "prox-token", "--force")
	if err != nil {
		t.Fatalf("forced rm: %v (%s)", err, out)
	}
	st2, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if _, err := st2.GetSecret(ctx, "prox-token"); err == nil {
		t.Error("secret still present after forced rm")
	}
}
