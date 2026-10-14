package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newSecretsCmd)
}

func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage sealed secrets",
	}
	cmd.AddCommand(newSecretsAddCmd(), newSecretsListCmd(),
		newSecretsReplaceCmd(), newSecretsRmCmd())
	return cmd
}

// generatedValueBytes is the entropy behind a --generate value: 32 random bytes
// (256 bits), rendered base64url so the sealed plaintext is printable env-safe
// ASCII.
const generatedValueBytes = 32

// maxSecretValueBytes is the sealed-secret value size ceiling.
const maxSecretValueBytes = 1 << 20

// readSecretValue reads a secret value from r, refusing a value at or over
// maxSecretValueBytes rather than silently sealing it truncated. It reads one
// byte past the limit — bounding the buffer instead of reading unboundedly —
// so the overrun is detected without ever holding more than the limit plus
// one byte in memory.
func readSecretValue(r io.Reader) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(r, maxSecretValueBytes+1))
	if err != nil {
		return nil, err
	}
	if len(value) > maxSecretValueBytes {
		return nil, fmt.Errorf("value exceeds the %d-byte limit", maxSecretValueBytes)
	}
	return value, nil
}

// generateSecretValue mints a cryptographically-random secret value.
func generateSecretValue() ([]byte, error) {
	raw := make([]byte, generatedValueBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate value: %w", err)
	}
	enc := base64.RawURLEncoding.EncodeToString(raw)
	return []byte(enc), nil
}

// parseExpiry turns a --expires flag into an absolute instant. An empty string
// means no expiry (nil). A value that parses as a Go duration (e.g. 720h) is
// taken relative to now; otherwise it must be an RFC3339 timestamp.
func parseExpiry(spec string) (*time.Time, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	if d, err := time.ParseDuration(spec); err == nil {
		if d <= 0 {
			return nil, fmt.Errorf("expiry duration %q is not in the future", spec)
		}
		t := time.Now().UTC().Add(d)
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, spec); err == nil {
		u := t.UTC()
		return &u, nil
	}
	return nil, fmt.Errorf("expiry %q is neither a Go duration (720h) nor an RFC3339 timestamp", spec)
}

// expirySuffix renders " (expires <RFC3339>)" for a set expiry, or "" for none.
func expirySuffix(at *time.Time) string {
	if at == nil {
		return ""
	}
	return " (expires " + at.UTC().Format(time.RFC3339) + ")"
}

func newSecretsAddCmd() *cobra.Command {
	var (
		kind     string
		generate bool
		expires  string
	)
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Seal a secret from stdin, or mint one with --generate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			expiresAt, err := parseExpiry(expires)
			if err != nil {
				return fmt.Errorf("secrets add: %w", err)
			}

			var (
				value  []byte
				source string
			)
			if generate {
				value, err = generateSecretValue()
				if err != nil {
					return fmt.Errorf("secrets add: %w", err)
				}
				source = store.SecretSourceGenerated
			} else {
				value, err = readSecretValue(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("secrets add: read value from stdin: %w", err)
				}
				value = []byte(strings.TrimRight(string(value), "\n"))
				if len(value) == 0 {
					return errors.New("secrets add: empty value on stdin")
				}
				source = store.SecretSourceUser
			}

			dir, err := dataDir()
			if err != nil {
				return err
			}
			env, err := secrets.Open(dir)
			if err != nil {
				return fmt.Errorf("secrets add: open envelope (run `exocomp init` first): %w", err)
			}
			defer func() { _ = env.Close() }()

			sealed, err := env.Seal(name, kind, value)
			if err != nil {
				return fmt.Errorf("secrets add: %w", err)
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if _, err := st.CreateSecret(cmd.Context(), store.SecretRow{
				Name:       sealed.Name,
				Kind:       sealed.Kind,
				Nonce:      sealed.Nonce,
				Ciphertext: sealed.Ciphertext,
				AAD:        sealed.Name + "|" + sealed.Kind,
				Source:     source,
				ExpiresAt:  expiresAt,
			}); err != nil {
				return fmt.Errorf("secrets add: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "sealed %q (kind %s, source %s, %d bytes)%s\n",
				name, kind, source, len(value), expirySuffix(expiresAt))
			return nil
		},
	}
	add.Flags().StringVar(&kind, "kind", "generic", "secret kind, bound into the AAD")
	add.Flags().BoolVar(&generate, "generate", false, "mint a random value instead of reading stdin")
	add.Flags().StringVar(&expires, "expires", "", "expiry as a Go duration (720h) or RFC3339 timestamp")
	return add
}

func newSecretsReplaceCmd() *cobra.Command {
	var (
		generate bool
		expires  string
	)
	replace := &cobra.Command{
		Use:     "replace <name>",
		Aliases: []string{"rotate"},
		Short:   "Replace a secret's sealed value (mints a fresh one for generated secrets)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			var expiresAt *time.Time
			reExpire := cmd.Flags().Changed("expires")
			if reExpire {
				var err error
				expiresAt, err = parseExpiry(expires)
				if err != nil {
					return fmt.Errorf("secrets replace: %w", err)
				}
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			existing, err := st.GetSecret(cmd.Context(), name)
			if err != nil {
				return fmt.Errorf("secrets replace: %w", err)
			}

			// A generated secret regenerates by default (nothing upstream holds the
			// old value); a user secret's new value comes from stdin. --generate
			// forces the mint path regardless of the recorded source.
			var (
				value       []byte
				fromStdin   bool
				userReplace bool
			)
			switch {
			case generate || existing.Source == store.SecretSourceGenerated:
				value, err = generateSecretValue()
				if err != nil {
					return fmt.Errorf("secrets replace: %w", err)
				}
			default:
				value, err = readSecretValue(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("secrets replace: read value from stdin: %w", err)
				}
				value = []byte(strings.TrimRight(string(value), "\n"))
				if len(value) == 0 {
					return errors.New("secrets replace: empty value on stdin (a user secret needs a new value)")
				}
				fromStdin = true
				userReplace = true
			}

			dir, err := dataDir()
			if err != nil {
				return err
			}
			env, err := secrets.Open(dir)
			if err != nil {
				return fmt.Errorf("secrets replace: open envelope: %w", err)
			}
			defer func() { _ = env.Close() }()

			// Reseal under the unchanged name|kind so the row still opens.
			sealed, err := env.Seal(existing.Name, existing.Kind, value)
			if err != nil {
				return fmt.Errorf("secrets replace: %w", err)
			}
			if err := st.RotateSecret(cmd.Context(), name, sealed.Nonce, sealed.Ciphertext); err != nil {
				return fmt.Errorf("secrets replace: %w", err)
			}
			if reExpire {
				if err := st.SetSecretExpiry(cmd.Context(), name, expiresAt); err != nil {
					return fmt.Errorf("secrets replace: %w", err)
				}
			}

			// The verb names what actually happened: a generated secret is
			// genuinely rotated (nothing upstream holds the old value), a user
			// secret's sealed copy is only replaced.
			verb := "replaced"
			if existing.Source == store.SecretSourceGenerated {
				verb = "rotated"
			}
			mode := "regenerated"
			if fromStdin {
				mode = "read from stdin"
			}
			suffix := ""
			if reExpire {
				suffix = expirySuffix(expiresAt)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %q (%s, %d bytes)%s\n",
				verb, name, mode, len(value), suffix)
			if userReplace {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
					"reminder: the PREVIOUS value may still be live upstream; revoking it there is yours to do.")
			}
			return nil
		},
	}
	replace.Flags().BoolVar(&generate, "generate", false, "mint a random value even for a user secret")
	replace.Flags().StringVar(&expires, "expires", "", "re-set expiry as a Go duration (720h) or RFC3339 timestamp; empty clears it")
	return replace
}

func newSecretsRmCmd() *cobra.Command {
	var force bool
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a sealed secret, guarded against bound run-secrets",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			refs, err := bindingsReferencing(cmd.Context(), st, name)
			if err != nil {
				return fmt.Errorf("secrets rm: %w", err)
			}
			if len(refs) > 0 {
				if err := confirmDestructive(cmd, force,
					fmt.Sprintf("secrets rm %q (bound by %d run-secret(s))", name, len(refs)),
					"--force"); err != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
						"refusing to delete %q: still bound by %d run-secret(s):\n", name, len(refs))
					for _, r := range refs {
						_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  binding %q env %s\n", r.Binding, r.EnvName)
					}
					return err
				}
			}

			if err := st.DeleteSecret(cmd.Context(), name); err != nil {
				return fmt.Errorf("secrets rm: %w", err)
			}
			forced := ""
			if len(refs) > 0 {
				forced = fmt.Sprintf(" (forced past %d binding reference(s))", len(refs))
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deleted %q%s\n", name, forced)
			return nil
		},
	}
	rm.Flags().BoolVar(&force, "force", false,
		"delete even when run-secret bindings reference the secret "+
			"(deprecated: prefer the global --assume-yes)")
	return rm
}

// bindingsReferencing returns every run_secret pair whose secret_name is name,
// across all bindings — the guard the delete path checks before removing a row.
func bindingsReferencing(ctx context.Context, st *store.Store, name string) ([]store.RunSecret, error) {
	bindings, err := st.ListRunSecretBindings(ctx)
	if err != nil {
		return nil, err
	}
	var refs []store.RunSecret
	for _, b := range bindings {
		pairs, err := st.RunSecretsByBinding(ctx, b)
		if err != nil {
			return nil, err
		}
		for _, p := range pairs {
			if p.SecretName == name {
				refs = append(refs, p)
			}
		}
	}
	return refs, nil
}

// expiryMarker renders a hygiene marker for the list view: EXPIRED for a past
// instant, "expires in Nd" for a future one, empty when there is no expiry.
func expiryMarker(at *time.Time, now time.Time) string {
	if at == nil {
		return ""
	}
	if !now.Before(*at) {
		return "EXPIRED"
	}
	d := at.Sub(now)
	if days := int(d.Hours()) / 24; days >= 1 {
		return fmt.Sprintf("expires in %dd", days)
	}
	if hours := int(d.Hours()); hours >= 1 {
		return fmt.Sprintf("expires in %dh", hours)
	}
	return "expires in <1h"
}

func newSecretsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List sealed secret names, sources, and expiry (never values)",
		Example: "exocomp secrets list --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			rows, err := st.ListSecrets(cmd.Context())
			if err != nil {
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindSecretList, wire.NewSecretList(rows))
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no sealed secrets")
				return nil
			}
			now := time.Now().UTC()
			for _, s := range rows {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-30s %-12s %-10s created %s  %s\n",
					s.Name, s.Kind, s.Source, s.CreatedAt.UTC().Format("2006-01-02 15:04"),
					expiryMarker(s.ExpiresAt, now))
			}
			return nil
		},
	}
}
