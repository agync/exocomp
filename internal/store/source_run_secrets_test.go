package store_test

import (
	"context"
	"errors"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// bindRunSecret registers one pair under binding so SetSourceRunSecrets has a
// real binding to accept.
func bindRunSecret(t *testing.T, s *store.Store, binding string) {
	t.Helper()
	if _, err := s.AddRunSecret(context.Background(), store.RunSecret{
		Binding: binding, EnvName: "VAULT_PASSWORD", SecretName: "vault-pass",
	}); err != nil {
		t.Fatalf("add run secret %q: %v", binding, err)
	}
}

func TestSourceDefaultRunSecretsDefaultEmpty(t *testing.T) {
	s, _ := open(t)
	src := newSource(t, s, "edge")
	if src.DefaultRunSecrets != "" {
		t.Fatalf("new source default run-secrets = %q, want empty", src.DefaultRunSecrets)
	}
}

func TestSetSourceRunSecretsPersistsPerSource(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	infra := newSource(t, s, "infra")
	edge := newSource(t, s, "edge")
	bindRunSecret(t, s, "vault")

	updated, err := s.SetSourceRunSecrets(ctx, infra.ID, "vault")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if updated.DefaultRunSecrets != "vault" {
		t.Fatalf("default run-secrets = %q, want vault", updated.DefaultRunSecrets)
	}
	got, err := s.SourceByName(ctx, "edge")
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	if got.DefaultRunSecrets != "" || got.ID != edge.ID {
		t.Fatalf("sibling default run-secrets = %q, want empty", got.DefaultRunSecrets)
	}

	cleared, err := s.SetSourceRunSecrets(ctx, infra.ID, "")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.DefaultRunSecrets != "" {
		t.Fatalf("after clear = %q, want empty", cleared.DefaultRunSecrets)
	}
}

// TestSetSourceRunSecretsRefusesUnknownBinding: a typo fails at registration,
// not when the source's next run fires, and nothing is written.
func TestSetSourceRunSecretsRefusesUnknownBinding(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	src := newSource(t, s, "edge")

	if _, err := s.SetSourceRunSecrets(ctx, src.ID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	got, err := s.GetSource(ctx, src.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DefaultRunSecrets != "" {
		t.Fatalf("after refused write = %q, want empty", got.DefaultRunSecrets)
	}
}

func TestSetSourceRunSecretsUnknownSource(t *testing.T) {
	s, _ := open(t)
	bindRunSecret(t, s, "vault")
	if _, err := s.SetSourceRunSecrets(context.Background(), 9999, "vault"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestDeleteRunSecretBindingRefusedWhileSourceDefault: removing a binding that a
// source names as its default is refused and leaves the pairs, and succeeds once
// the default is cleared.
func TestDeleteRunSecretBindingRefusedWhileSourceDefault(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	src := newSource(t, s, "infra")
	bindRunSecret(t, s, "vault")
	if _, err := s.SetSourceRunSecrets(ctx, src.ID, "vault"); err != nil {
		t.Fatalf("set: %v", err)
	}

	if err := s.DeleteRunSecretBinding(ctx, "vault"); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("delete err = %v, want ErrValidation", err)
	}
	pairs, err := s.RunSecretsByBinding(ctx, "vault")
	if err != nil || len(pairs) != 1 {
		t.Fatalf("pairs after refused delete = %v, %v; want 1", pairs, err)
	}

	if _, err := s.SetSourceRunSecrets(ctx, src.ID, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := s.DeleteRunSecretBinding(ctx, "vault"); err != nil {
		t.Fatalf("delete after clear: %v", err)
	}
}
