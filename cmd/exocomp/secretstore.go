package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newSecretsCmd)
}

func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage sealed secrets",
	}
	cmd.AddCommand(newSecretsAddCmd(), newSecretsListCmd())
	return cmd
}

func newSecretsAddCmd() *cobra.Command {
	var kind string
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Seal a secret read from stdin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			dir, err := dataDir()
			if err != nil {
				return err
			}
			env, err := secrets.Open(dir)
			if err != nil {
				return fmt.Errorf("secrets add: open envelope (run `exocomp init` first): %w", err)
			}
			defer func() { _ = env.Close() }()

			value, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			if err != nil {
				return fmt.Errorf("secrets add: read value from stdin: %w", err)
			}
			value = []byte(strings.TrimRight(string(value), "\n"))
			if len(value) == 0 {
				return errors.New("secrets add: empty value on stdin")
			}

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
			}); err != nil {
				return fmt.Errorf("secrets add: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "sealed %q (kind %s, %d bytes)\n", name, kind, len(value))
			return nil
		},
	}
	add.Flags().StringVar(&kind, "kind", "generic", "secret kind, bound into the AAD")
	return add
}

func newSecretsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List sealed secret names (never values)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			rows, err := st.ListSecrets(cmd.Context())
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no sealed secrets")
				return nil
			}
			for _, s := range rows {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-30s %-12s created %s\n", s.Name, s.Kind, s.CreatedAt.UTC().Format("2006-01-02 15:04"))
			}
			return nil
		},
	}
}
