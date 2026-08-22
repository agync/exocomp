package main

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/web"
)

func init() {
	registerCommand(newTokenCmd)
}

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage LAN access tokens (ADR 0009)",
		Long: "LAN access tokens are the only credential the network surface accepts. " +
			"They are minted from local auth — this command, which needs the data dir — " +
			"argon2id-hashed at rest, and revocable. A browser session is bound to its " +
			"token, so revoking the token kills its sessions.",
	}
	cmd.AddCommand(newTokenNewCmd(), newTokenListCmd(), newTokenRevokeCmd())
	return cmd
}

func newTokenNewCmd() *cobra.Command {
	var scope string
	var expires time.Duration

	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Mint a token and print it once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			switch scope {
			case store.ScopeOperator, store.ScopeReadonly, store.ScopeApprove:
			default:
				return fmt.Errorf("token new: --scope must be %s, %s, or %s",
					store.ScopeOperator, store.ScopeReadonly, store.ScopeApprove)
			}

			dir, err := dataDir()
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			token, row, err := web.MintToken(cmd.Context(), st, name, scope, expires)
			if errors.Is(err, store.ErrDuplicate) {
				return fmt.Errorf("token new: a token named %q already exists", name)
			}
			if err != nil {
				return fmt.Errorf("token new: %w", err)
			}

			// The self-signed certificate is generated now, so the operator has
			// the fingerprint to pin before the first browser connects.
			fingerprint, err := web.Fingerprint(dir)
			if err != nil {
				return fmt.Errorf("token new: %w", err)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "token %q (scope %s) — copy it now, it is not shown again:\n\n  %s\n\n",
				row.Name, row.Scope, token)
			if row.ExpiresAt != nil {
				_, _ = fmt.Fprintf(out, "expires %s\n", row.ExpiresAt.Format(time.RFC3339))
			}
			_, _ = fmt.Fprintf(out, "certificate SHA-256 (pin this on first connect):\n  %s\n", fingerprint)
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", store.ScopeOperator,
		"token scope: operator, readonly, or approve")
	cmd.Flags().DurationVar(&expires, "expires", 0,
		"how long the token is valid (0: never expires)")
	return cmd
}

func newTokenListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tokens (never the secret)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListTokens(cmd.Context(), all)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no tokens")
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tSCOPE\tPREFIX\tEXPIRES\tLAST USED\tSTATE")
			now := time.Now().UTC()
			for _, t := range rows {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s…\t%s\t%s\t%s\n",
					t.Name, t.Scope, t.Prefix,
					stampOrDash(t.ExpiresAt), stampOrDash(t.LastUsedAt), tokenState(t, now))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include revoked tokens")
	return cmd
}

func newTokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke a token immediately, killing its sessions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.RevokeToken(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("token revoke: no active token named %q", args[0])
				}
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "revoked %q; its sessions are dead\n", args[0])
			return nil
		},
	}
}

func stampOrDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func tokenState(t store.APIToken, now time.Time) string {
	switch {
	case t.RevokedAt != nil:
		return "revoked"
	case t.ExpiresAt != nil && !now.Before(*t.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}
