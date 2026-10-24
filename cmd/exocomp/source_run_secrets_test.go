package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// bindVaultRunSecret seals value and binds it as VAULT_PASSWORD under binding,
// through the same CLI an operator uses.
func bindVaultRunSecret(t *testing.T, dir, binding, secretName, value string) {
	t.Helper()
	sealSecretDirect(t, dir, secretName, value)
	if out, err := runRunSecret(t, "add", binding, "VAULT_PASSWORD", secretName); err != nil {
		t.Fatalf("run-secret add: %v (%s)", err, out)
	}
}

// TestSourceRunSecretsSetsAndClearsPerSource: the default is recorded on the one
// source named, echoed back, and cleared by omitting the binding.
func TestSourceRunSecretsSetsAndClearsPerSource(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()
	bindVaultRunSecret(t, dir, "tree-vault", "tree-vault-pass", "value-one")

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, name := range []string{"infra", "edge"} {
		if _, err := st.CreateSource(ctx, store.Source{Name: name, Kind: store.SourceGit, Enabled: true}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	_ = st.Close()

	out, err := runCobra(t, newSourceCmd, "run-secrets", "infra", "tree-vault")
	if err != nil {
		t.Fatalf("source run-secrets: %v (%s)", err, out)
	}
	if !strings.Contains(out, "tree-vault") {
		t.Fatalf("output = %q, want the binding name", out)
	}

	check := func(name, want string) {
		t.Helper()
		st, err := store.Open(filepath.Join(dir, "exocomp.db"))
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		defer func() { _ = st.Close() }()
		src, err := st.SourceByName(ctx, name)
		if err != nil {
			t.Fatalf("by name %s: %v", name, err)
		}
		if src.DefaultRunSecrets != want {
			t.Fatalf("%s default run-secrets = %q, want %q", name, src.DefaultRunSecrets, want)
		}
	}
	check("infra", "tree-vault")
	check("edge", "")

	if out, err := runCobra(t, newSourceCmd, "run-secrets", "infra"); err != nil {
		t.Fatalf("clear: %v (%s)", err, out)
	}
	check("infra", "")
}

// TestSourceRunSecretsRefusesUnknownInputs: a missing source or a binding with
// no pairs fails at registration, before any run could inherit it.
func TestSourceRunSecretsRefusesUnknownInputs(t *testing.T) {
	dir := initForSource(t)
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateSource(context.Background(),
		store.Source{Name: "infra", Kind: store.SourceGit, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = st.Close()

	if _, err := runCobra(t, newSourceCmd, "run-secrets", "infra", "typo"); err == nil {
		t.Fatal("an unbound binding was accepted")
	}
	if _, err := runCobra(t, newSourceCmd, "run-secrets", "ghost", "typo"); err == nil {
		t.Fatal("an unknown source was accepted")
	}
}
