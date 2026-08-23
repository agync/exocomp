package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// SecretKindTofuOutput is the kind bound into the AAD of a sealed sensitive
// tofu output. It marks the row's provenance and, like every kind, is part of
// what a moved ciphertext fails to open against.
const SecretKindTofuOutput = "tofu-output"

// TofuOutputBinding is the run-secret binding a workspace's sealed sensitive
// outputs are collected under. It is deliberately the same name as the standing
// inventory a workspace's apply refreshes (`tf:<workspace>`): a play that
// targets that inventory resolves the binding of the same name, which is what
// carries a sealed output into the dependent play with nothing for an operator
// to copy.
func TofuOutputBinding(workspace string) string {
	return "tf:" + workspace
}

// TofuOutputEnvName derives the environment variable name a sealed output binds
// to: the declared output name upper-cased, and nothing else. Case folding is
// not mangling; substituting characters would be, so a name that is not a POSIX
// identifier after folding (a dash, a dot — both legal in a Terraform output
// name) is refused here rather than rewritten into something the operator never
// wrote. Renaming the output, or binding it by hand, is their call.
func TofuOutputEnvName(output string) (string, bool) {
	name := strings.ToUpper(output)
	if !isEnvName(name) {
		return "", false
	}
	return name, true
}

// EnvelopeOutputSealer is the OutputSealer that routes a sensitive tofu output
// into the secret store: seal under the envelope DEK, persist the ciphertext,
// then bind it as a named env var for the dependent play. It is the glue
// between the exit-code-only executor and the credential plane; the tofu driver
// holds only the interface, never the store.
//
// Both halves are idempotent by name: a re-apply reseals the same
// tf-output/<workspace>/<name> row (rotate in place) and finds its binding pair
// already present, so the latest apply's value is what the next play resolves
// and no re-apply duplicates a row.
type EnvelopeOutputSealer struct {
	Env   *secrets.Envelope
	Store *store.Store
	// Workspace names the state the outputs came from. It is what makes both
	// names deterministic — the secret row's and the binding's — so a producer
	// and a consumer agree on them without a round-trip.
	Workspace string
}

// SealOutput implements OutputSealer.
func (s EnvelopeOutputSealer) SealOutput(ctx context.Context, output string, value []byte) (string, error) {
	if s.Env == nil || s.Store == nil {
		return "", errors.New("executor: output sealer missing envelope or store")
	}
	if s.Workspace == "" {
		return "", errors.New("executor: output sealer missing workspace")
	}
	secretName := TofuOutputSecretName(s.Workspace, output)
	sealed, err := s.Env.Seal(secretName, SecretKindTofuOutput, value)
	if err != nil {
		return "", fmt.Errorf("executor: seal tofu output: %w", err)
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
		return "", fmt.Errorf("executor: persist sealed tofu output: %w", err)
	}
	return s.bind(ctx, output, sealed.Name)
}

// bind maps the sealed row to an env name under the workspace's binding, and
// reports why it did not when it did not. A binding is (env name -> secret
// name) only: the value stays in the envelope and is opened per run.
//
// It never overwrites a pair that already points somewhere else. The secret
// name is deterministic, so the only way to reach that branch is a collision an
// operator would not expect — two outputs folding to one env name, or their own
// hand-made binding under the same name — and silently repointing DB_PASSWORD
// at a different secret is worse than refusing one output's bind.
func (s EnvelopeOutputSealer) bind(ctx context.Context, output, secretName string) (string, error) {
	envName, ok := TofuOutputEnvName(output)
	if !ok {
		return output + ": not bound, an env name must be a POSIX identifier", nil
	}
	binding := TofuOutputBinding(s.Workspace)
	pairs, err := s.Store.RunSecretsByBinding(ctx, binding)
	if err != nil {
		return "", fmt.Errorf("executor: read binding %q: %w", binding, err)
	}
	for _, p := range pairs {
		if p.EnvName != envName {
			continue
		}
		if p.SecretName == secretName {
			return "", nil
		}
		return fmt.Sprintf("%s: not bound, %s already binds %s", output, envName, p.SecretName), nil
	}
	_, err = s.Store.AddRunSecret(ctx, store.RunSecret{
		Binding:    binding,
		EnvName:    envName,
		SecretName: secretName,
	})
	// A concurrent apply of the same workspace can win the insert; the pair it
	// wrote is the one this apply would have written, so the bind is done.
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		return "", fmt.Errorf("executor: bind sealed tofu output: %w", err)
	}
	return "", nil
}
