package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/service"
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

// readSecretValue reads a secret value from r, refusing a value over
// service.MaxSecretValueBytes rather than silently sealing it truncated. It
// reads one byte past the limit, so the overrun is detected without holding more
// than the limit plus one byte in memory. The trailing newline a shell adds is
// trimmed, and an empty value is refused.
func readSecretValue(r io.Reader) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(r, service.MaxSecretValueBytes+1))
	if err != nil {
		return nil, err
	}
	if len(value) > service.MaxSecretValueBytes {
		return nil, fmt.Errorf("value exceeds the %d-byte limit", service.MaxSecretValueBytes)
	}
	value = []byte(strings.TrimRight(string(value), "\n"))
	if len(value) == 0 {
		return nil, service.ErrEmptySecretValue
	}
	return value, nil
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

// formatExpiry renders an expiry as the RFC3339 text the API carries, or "".
func formatExpiry(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// parseWireTime reads an optional RFC3339 wire timestamp; an unparseable one
// reads as none, since it only feeds display.
func parseWireTime(v *string) *time.Time {
	if v == nil || *v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *v)
	if err != nil {
		return nil
	}
	return &t
}

// printSecretSealed reports a sealed secret, by name and size, never by value.
func printSecretSealed(cmd *cobra.Command, name, kind, source string, size int, expiresAt *time.Time) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "sealed %q (kind %s, source %s, %d bytes)%s\n",
		name, kind, source, size, expirySuffix(expiresAt))
}

// printSecretReplaced names what actually happened: a generated secret is
// rotated (nothing upstream holds the old value), a user secret's sealed copy is
// only replaced, and its previous value may still be live upstream.
func printSecretReplaced(cmd *cobra.Command, name string, rotated, regenerated bool, size int,
	setExpiry bool, expiresAt *time.Time) {
	verb, mode := "replaced", "read from stdin"
	if rotated {
		verb = "rotated"
	}
	if regenerated {
		mode = "regenerated"
	}
	suffix := ""
	if setExpiry {
		suffix = expirySuffix(expiresAt)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %q (%s, %d bytes)%s\n", verb, name, mode, size, suffix)
	if !regenerated {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"reminder: the PREVIOUS value may still be live upstream; revoking it there is yours to do.")
	}
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
			var value []byte
			if !generate {
				if value, err = readSecretValue(cmd.InOrStdin()); err != nil {
					return fmt.Errorf("secrets add: read value from stdin: %w", err)
				}
			}
			defer zero(value)

			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				return runRemoteSecretAdd(cmd, rc, client.SecretAddRequest{
					Name: name, Kind: kind, Value: value, Generate: generate,
					ExpiresAt: formatExpiry(expiresAt),
				})
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
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			res, err := service.New(st, dir, nil).SealSecret(cmd.Context(), env, service.SealSecretInput{
				Name: name, Kind: kind, Value: value, Generate: generate, ExpiresAt: expiresAt,
			})
			if err != nil {
				return fmt.Errorf("secrets add: %w", err)
			}
			printSecretSealed(cmd, name, res.Row.Kind, res.Row.Source, res.Bytes, res.Row.ExpiresAt)
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

			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				return runRemoteSecretReplace(cmd, rc, name, generate, reExpire, expiresAt)
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
			var value []byte
			if !generate && existing.Source != store.SecretSourceGenerated {
				if value, err = readSecretValue(cmd.InOrStdin()); err != nil {
					return fmt.Errorf("secrets replace: read value from stdin (a user secret needs a new value): %w", err)
				}
				defer zero(value)
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

			res, err := service.New(st, dir, nil).ReplaceSecret(cmd.Context(), env, service.ReplaceSecretInput{
				Name: name, Value: value, Generate: generate, SetExpiry: reExpire, ExpiresAt: expiresAt,
			})
			if err != nil {
				return fmt.Errorf("secrets replace: %w", err)
			}
			printSecretReplaced(cmd, name, res.Rotated, res.Regenerated, res.Bytes, res.SetExpiry, res.ExpiresAt)
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
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				return runRemoteSecretRm(cmd, rc, name, force)
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			refs, err := service.New(st, "", nil).SecretBindings(cmd.Context(), name)
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
			var views []wire.SecretView
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				if views, err = rc.ListSecrets(cmd.Context()); err != nil {
					return remoteFail(cmd, mode, err)
				}
			} else {
				st, err := openStore()
				if err != nil {
					return err
				}
				defer func() { _ = st.Close() }()
				rows, err := st.ListSecrets(cmd.Context())
				if err != nil {
					return err
				}
				views = wire.NewSecretList(rows)
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindSecretList, views)
			}
			if len(views) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no sealed secrets")
				return nil
			}
			now := time.Now().UTC()
			for _, s := range views {
				created, _ := time.Parse(time.RFC3339, s.CreatedAt)
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-30s %-12s %-10s created %s  %s\n",
					s.Name, s.Kind, s.Source, created.UTC().Format("2006-01-02 15:04"),
					expiryMarker(parseWireTime(s.ExpiresAt), now))
			}
			return nil
		},
	}
}
