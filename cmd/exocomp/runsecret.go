package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// resolveRunSecrets builds a run's credential-plane environment: the sealed
// values a named run_secret binding maps to env names, plus a dynamic
// inventory's env_binding, decrypted through the envelope at execution time.
// This is the convergent must-fix (docs/design/expert-reviews.md) — the env a
// provider or dynamic-inventory plugin authenticates from, injected for one run
// so the sandbox allowlist that would otherwise strip it does not starve it.
//
// It returns the plaintexts too, so the run's redactor scrubs them from events;
// the values never touch the job row, which carries only the binding name.
// A nil envelope yields nothing: without it no sealed value can be opened.
func resolveRunSecrets(ctx context.Context, st *store.Store, env *secrets.Envelope,
	binding, inventory string) (executor.RunSecrets, [][]byte, error) {
	if env == nil || (binding == "" && inventory == "") {
		return nil, nil, nil
	}

	out := executor.RunSecrets{}
	var plaintexts [][]byte
	add := func(envName, secretName string) error {
		row, err := st.GetSecret(ctx, secretName)
		if err != nil {
			return fmt.Errorf("run secret %s -> %s: %w", envName, secretName, err)
		}
		// Warn, never block: an expired secret is still injected so the run gets
		// its value. Blocking here would let clock skew strand a run over a
		// deadline the operator can only see after the fact. The name and deadline
		// are logged; the value never is (AGENTS.md invariant 4).
		if secretExpired(row, time.Now().UTC()) {
			slog.Warn("run secret is past its expiry but still injected; rotate it",
				"secret", row.Name, "kind", row.Kind,
				"expired_at", row.ExpiresAt.UTC().Format(time.RFC3339))
		}
		pt, err := env.OpenSecret(secrets.SealedSecret{
			Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
		})
		if err != nil {
			return fmt.Errorf("run secret %s: %w", envName, err)
		}
		out[envName] = string(pt)
		plaintexts = append(plaintexts, pt)
		return nil
	}

	if binding != "" {
		pairs, err := st.RunSecretsByBinding(ctx, binding)
		if err != nil {
			return nil, nil, err
		}
		if len(pairs) == 0 {
			return nil, nil, fmt.Errorf("run secret binding %q: %w", binding, store.ErrNotFound)
		}
		for _, p := range pairs {
			if err := add(p.EnvName, p.SecretName); err != nil {
				return nil, nil, err
			}
		}
	}

	if inventory != "" {
		inv, err := st.InventoryByName(ctx, inventory)
		if err != nil {
			return nil, nil, err
		}
		for envName, secretName := range inv.EnvBinding {
			if err := add(envName, secretName); err != nil {
				return nil, nil, err
			}
		}
	}

	if len(out) == 0 {
		return nil, plaintexts, nil
	}
	return out, plaintexts, nil
}

// secretExpired reports whether a sealed secret's rotation deadline has passed.
// A secret with no deadline never expires.
func secretExpired(row store.SecretRow, now time.Time) bool {
	return row.ExpiresAt != nil && now.After(*row.ExpiresAt)
}
