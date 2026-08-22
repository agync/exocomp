package executor

import (
	"context"
	"errors"
	"fmt"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// SecretKindTofuOutput is the kind bound into the AAD of a sealed sensitive
// tofu output. It marks the row's provenance and, like every kind, is part of
// what a moved ciphertext fails to open against.
const SecretKindTofuOutput = "tofu-output"

// EnvelopeOutputSealer is the OutputSealer that routes a sensitive tofu output
// into the secret store: seal under the envelope DEK, then persist the
// ciphertext as a run-secret row a later step binds as named env. It is the
// glue between the exit-code-only executor and the credential plane; the tofu
// driver holds only the interface, never the store.
//
// Sealing is idempotent by name: a re-apply reseals the same
// tf-output/<workspace>/<name> row (rotate in place) rather than failing on the
// unique-name constraint, so the latest apply's value is what a downstream bind
// resolves.
type EnvelopeOutputSealer struct {
	Env   *secrets.Envelope
	Store *store.Store
}

// SealOutput implements OutputSealer.
func (s EnvelopeOutputSealer) SealOutput(ctx context.Context, name string, value []byte) error {
	if s.Env == nil || s.Store == nil {
		return errors.New("executor: output sealer missing envelope or store")
	}
	sealed, err := s.Env.Seal(name, SecretKindTofuOutput, value)
	if err != nil {
		return fmt.Errorf("executor: seal tofu output: %w", err)
	}
	// Rotate-then-create is the upsert: an existing row (a prior apply) rotates
	// in place; a first apply creates it. Neither path carries plaintext.
	err = s.Store.RotateSecret(ctx, sealed.Name, sealed.Nonce, sealed.Ciphertext)
	if errors.Is(err, store.ErrNotFound) {
		_, err = s.Store.CreateSecret(ctx, store.SecretRow{
			Name:       sealed.Name,
			Kind:       sealed.Kind,
			Nonce:      sealed.Nonce,
			Ciphertext: sealed.Ciphertext,
			AAD:        sealed.Name + "|" + sealed.Kind,
		})
	}
	if err != nil {
		return fmt.Errorf("executor: persist sealed tofu output: %w", err)
	}
	return nil
}
