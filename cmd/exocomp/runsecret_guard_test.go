package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

func TestLooksLikeAccessToken(t *testing.T) {
	cases := map[string]bool{
		"exo_1_abcdef":       true,
		"exo_42_a-b_c":       true,  // secret may itself contain underscores
		"exo_0_x":            false, // id must be positive
		"exo_-3_x":           false,
		"exo__x":             false, // empty id
		"exo_7_":             false, // empty secret
		"exo_notanum_x":      false,
		"proxmox-token":      false,
		"https://host/x.git": false,
		"":                   false,
		"exo_7":              false, // too few parts
	}
	for v, want := range cases {
		if got := looksLikeAccessToken(v); got != want {
			t.Errorf("looksLikeAccessToken(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestResolveRunSecretsRefusesAccessToken proves the injection path refuses a
// sealed value shaped like an exocomp access token — an operator binding the
// daemon's own credential as a run-secret is the one unmodeled confused-deputy
// path, and it is refused before the value reaches a job's env.
func TestResolveRunSecretsRefusesAccessToken(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	sealSecretDirect(t, dir, "leaky", "exo_7_S3cr3tTokenBytes")
	if out, err := runRunSecret(t, "add", "b", "EXO_TOKEN", "leaky"); err != nil {
		t.Fatalf("run-secret add: %v (%s)", err, out)
	}
	// A normal secret in the same binding still injects.
	sealSecretDirect(t, dir, "ok", "ordinary-value")
	if out, err := runRunSecret(t, "add", "b", "NORMAL", "ok"); err != nil {
		t.Fatalf("run-secret add: %v (%s)", err, out)
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()

	_, _, err = resolveRunSecrets(ctx, st, env, "b", "")
	if err == nil {
		t.Fatal("expected a refusal to inject an access-token-shaped value")
	}
	if strings.Contains(err.Error(), "S3cr3tTokenBytes") {
		t.Fatalf("error must not leak the value (invariant 4): %v", err)
	}
}
