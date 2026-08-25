package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// resolveRunSecrets builds a run's credential-plane environment: the sealed
// values a named run_secret binding maps to env names, plus a dynamic
// inventory's env_binding, decrypted through the envelope at execution time.
// It carries the env a provider or dynamic-inventory plugin authenticates
// from, injected for one run so the sandbox allowlist that would otherwise
// strip it does not starve it.
//
// It returns the plaintexts too, so the run's redactor scrubs them from events;
// the values never touch the job row, which carries only the binding name.
// A nil envelope yields nothing: without it no sealed value can be opened.
//
// Four sources, lowest precedence first: the workspace binding a `tf:` target
// implies (see autoTofuOutputs), the `ab:<workflow>` binding a bootstrap chain's
// play produced for its own tofu step (see autoAnsibleOutputs), the explicitly
// named binding, then the dynamic inventory's env_binding. An operator's explicit
// binding therefore wins over one an apply or a play produced, the way round that
// lets them override it.
// looksLikeAccessToken reports whether v has the shape of an exocomp LAN access
// token, exo_<id>_<secret> (mirrors the web package's token format). A sealed
// secret value never legitimately looks like this, so a match means an operator
// bound the daemon's own credential as a run-secret — which the injection path
// refuses.
func looksLikeAccessToken(v string) bool {
	parts := strings.SplitN(v, "_", 3)
	if len(parts) != 3 || parts[0] != "exo" || parts[2] == "" {
		return false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && id > 0
}

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
		// Refuse to hand a job the daemon's own access token. A value shaped like
		// exo_<id>_<secret> bound as a run-secret would inject an API credential
		// into the run's env — a confused-deputy path a sandboxed job could use to
		// drive the daemon's API (see the threat model). The value itself is never
		// named in the error (AGENTS.md invariant 4).
		if looksLikeAccessToken(string(pt)) {
			return fmt.Errorf("run secret %s -> %s: refusing to inject a value shaped like an exocomp access token; an API token must not be bound as a run-secret", envName, secretName)
		}
		out[envName] = string(pt)
		plaintexts = append(plaintexts, pt)
		return nil
	}

	auto, err := autoTofuOutputs(ctx, st, inventory, binding)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range auto {
		if err := add(p.EnvName, p.SecretName); err != nil {
			return nil, nil, err
		}
	}

	ab, err := autoAnsibleOutputs(ctx, st, binding)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range ab {
		if err := add(p.EnvName, p.SecretName); err != nil {
			return nil, nil, err
		}
	}

	// An `ab:<workflow>` binding is an auto binding, resolved empty-tolerantly by
	// autoAnsibleOutputs above; only a non-auto name is an operator's explicit
	// binding that must exist. Resolving it here too would double-add its pairs and
	// turn an empty capture (a play that sealed nothing) into a hard error.
	if binding != "" && !strings.HasPrefix(binding, ansibleOutputBindingPrefix) {
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

// autoTofuOutputs returns the run-secrets a run inherits from the workspace it
// is targeting: when the inventory is a workspace's standing `tf:<workspace>`
// inventory, the binding of the same name — the sensitive tofu outputs that
// workspace's last apply sealed and bound. This is what closes the loop the
// sealing fold opened: the configure play in a workspace's own provision chain
// receives $DB_PASSWORD with nothing copied by an operator.
//
// It is scoped by that target, not global. A run naming another inventory, a
// static one, or none at all inherits nothing, so a workspace's sealed outputs
// cannot reach a run that has nothing to do with it. An empty binding is not an
// error either: most workspaces seal no outputs, and a run must not start
// failing because one of them does not.
func autoTofuOutputs(ctx context.Context, st *store.Store, inventory, binding string) ([]store.RunSecret, error) {
	workspace, ok := strings.CutPrefix(inventory, handoffInventoryPrefix)
	if !ok || workspace == "" {
		return nil, nil
	}
	// The producer names the binding after the workspace, so derive it the same
	// way rather than reusing the inventory name verbatim.
	auto := executor.TofuOutputBinding(workspace)
	if auto == binding {
		return nil, nil // the explicit binding already covers it
	}
	return st.RunSecretsByBinding(ctx, auto)
}

// ansibleOutputBindingPrefix marks a run-secret binding a bootstrap chain's play
// produced: `ab:<workflow>`. It mirrors handoffInventoryPrefix ("tf:") and must
// match executor.AnsibleOutputBinding, which builds "ab:"+workflow.
const ansibleOutputBindingPrefix = "ab:"

// autoAnsibleOutputs resolves the sealed captures a bootstrap chain's play bound
// under ab:<workflow>, for the tofu step of that same chain and nothing else. The
// composer names the tofu step's binding ab:<workflow> directly, so this
// recognises that prefix and resolves it empty-tolerantly: a chain whose play
// captured nothing, or whose captures folded to a non-POSIX env name and were not
// bound, leaves the binding empty and the apply inherits no env — never an error,
// exactly as autoTofuOutputs tolerates a workspace that sealed no outputs.
//
// Scope is the binding string itself: only a step whose binding is literally
// ab:<this-workflow> resolves this workflow's captures, so an unrelated tofu run —
// another workflow, a bare apply, none at all — inherits nothing. A binding
// without the ab: prefix is an operator's explicit binding, not this to resolve.
func autoAnsibleOutputs(ctx context.Context, st *store.Store, binding string) ([]store.RunSecret, error) {
	if !strings.HasPrefix(binding, ansibleOutputBindingPrefix) {
		return nil, nil
	}
	return st.RunSecretsByBinding(ctx, binding)
}

// secretExpired reports whether a sealed secret's rotation deadline has passed.
// A secret with no deadline never expires.
func secretExpired(row store.SecretRow, now time.Time) bool {
	return row.ExpiresAt != nil && now.After(*row.ExpiresAt)
}
