package main

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/pairing"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/web"
	"exocomp.dev/exocomp/internal/wire"
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
	cmd.AddCommand(newTokenNewCmd(), newTokenListCmd(), newTokenRevokeCmd(), newTokenRotateCmd())
	return cmd
}

func newTokenNewCmd() *cobra.Command {
	var scope string
	var expires time.Duration
	var showQR bool

	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Mint a token and print it once, with a pairing QR",
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
				return duplicateTokenError(cmd, st, name)
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

			if showQR {
				if err := printPairingQR(cmd, st, token, row, fingerprint); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", store.ScopeOperator,
		"token scope: operator, readonly, or approve")
	cmd.Flags().DurationVar(&expires, "expires", 0,
		"how long the token is valid (0: never expires)")
	cmd.Flags().BoolVar(&showQR, "qr", true,
		"print a scannable pairing QR for the phone companion (--qr=false to omit)")
	return cmd
}

// pairURLPlaceholder stands in the pairing payload when daemon.public_url is
// unset: the QR still encodes, and the operator edits the host after scanning.
const pairURLPlaceholder = "https://EXOCOMP-PUBLIC-URL-NOT-SET"

// printPairingQR renders the same device-pairing payload the web result page
// shows as a scannable terminal QR plus the essentials in text. The payload
// carries the token, so it is never logged — only written to the command's
// output for the operator at the keyboard.
func printPairingQR(cmd *cobra.Command, st *store.Store, token string, row store.APIToken, fingerprint string) error {
	base, err := st.GetSetting(cmd.Context(), store.SettingPublicURL)
	unset := false
	if errors.Is(err, store.ErrNotFound) || base == "" {
		base, unset = pairURLPlaceholder, true
	} else if err != nil {
		return fmt.Errorf("token new: %w", err)
	}

	p := pairing.New(base, token, fingerprint, row.Scope, row.Name)
	qr, err := p.Terminal()
	if err != nil {
		return fmt.Errorf("token new: %w", err)
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "\nScan to pair a device (%s scope):\n\n%s\n", row.Scope, qr)
	_, _ = fmt.Fprintf(out, "  url:         %s\n", base)
	if unset {
		_, _ = fmt.Fprintln(out, "               (daemon.public_url unset — set it with `exocomp serve --public-url` or on /system)")
	}
	_, _ = fmt.Fprintf(out, "  fingerprint: %s\n", fingerprint)
	_, _ = fmt.Fprintf(out, "  scope:       %s\n", row.Scope)
	_, _ = fmt.Fprintf(out, "  name:        %s\n", row.Name)
	return nil
}

func newTokenListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List tokens (never the secret)",
		Example: "exocomp token list --output json",
		Args:    cobra.NoArgs,
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

			rows, err := st.ListTokens(cmd.Context(), all)
			if err != nil {
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindTokenList, wire.NewTokenList(rows))
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

func newTokenRotateCmd() *cobra.Command {
	var expires time.Duration
	var setExpiry bool

	cmd := &cobra.Command{
		Use:   "rotate <name>",
		Short: "Reseal a token with a fresh secret, keeping its name and scope",
		Long: "Rotate issues a new secret for an existing token, keeping its id, name, " +
			"and scope so audit and attribution stay continuous. The old secret stops " +
			"working and its sessions are dropped; a revoked token is brought back live. " +
			"This is the way to reuse a name — a revoked name stays reserved and cannot be " +
			"re-minted.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			setExpiry = cmd.Flags().Changed("expires")

			dir, err := dataDir()
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			token, row, err := web.RotateToken(cmd.Context(), st, name, expires, setExpiry)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("token rotate: no token named %q; mint one with `exocomp token new %s`", name, name)
			}
			if err != nil {
				return fmt.Errorf("token rotate: %w", err)
			}

			fingerprint, err := web.Fingerprint(dir)
			if err != nil {
				return fmt.Errorf("token rotate: %w", err)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "rotated %q (scope %s) — copy the new secret now, it is not shown again:\n\n  %s\n\n",
				row.Name, row.Scope, token)
			if row.ExpiresAt != nil {
				_, _ = fmt.Fprintf(out, "expires %s\n", row.ExpiresAt.Format(time.RFC3339))
			}
			_, _ = fmt.Fprintf(out, "certificate SHA-256 (pin this on first connect):\n  %s\n", fingerprint)
			return nil
		},
	}
	cmd.Flags().DurationVar(&expires, "expires", 0,
		"reset how long the token is valid (0: never expires); omit to keep the current expiry")
	return cmd
}

// duplicateTokenError explains a name collision on `token new`: the holder is
// either an active token or a revoked one whose name stays reserved. Both point
// at rotate as the way to reissue the name.
func duplicateTokenError(cmd *cobra.Command, st *store.Store, name string) error {
	existing, err := st.GetTokenByName(cmd.Context(), name)
	if err != nil {
		// Fall back to the plain message if the row cannot be read back.
		return fmt.Errorf("token new: a token named %q already exists", name)
	}
	if existing.RevokedAt != nil {
		return fmt.Errorf("token new: a revoked token named %q still holds the name "+
			"(revoked names stay reserved for audit and attribution; see `exocomp token list --all`). "+
			"Pick a different name, or `exocomp token rotate %s` to reissue it", name, name)
	}
	return fmt.Errorf("token new: a token named %q already exists; "+
		"`exocomp token rotate %s` reissues its secret, or revoke it first", name, name)
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
