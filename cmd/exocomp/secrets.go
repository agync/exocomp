package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/secrets"
)

func init() {
	registerCommand(newInitCmd)
	registerCommand(newRecoveryKitCmd)
}

func newInitCmd() *cobra.Command {
	var extra []string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the host identity and keyring, print the recovery kit",
		Long: "Generates the X25519 host identity and a recovery identity, wraps a fresh " +
			"data encryption key to both, and prints the recovery kit. The recovery secret " +
			"key is printed once and never written to disk.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir()
			if err != nil {
				return fmt.Errorf("init: resolve data dir: %w", err)
			}

			env, kit, err := secrets.Init(dir, extra)
			if err != nil {
				if errors.Is(err, secrets.ErrAlreadyInitialized) {
					return fmt.Errorf("init: %s is already initialized; "+
						"run `exocomp recovery-kit` for the kit structure, or re-key to mint "+
						"a new recovery identity", dir)
				}
				return err
			}
			defer func() { _ = env.Close() }()

			warnIfPlaintextHostKey(env.HostKeyProtection(), cmd.ErrOrStderr())
			decorateKit(cmd.Context(), kit, cmd.ErrOrStderr())
			_, err = io.WriteString(cmd.OutOrStdout(), kit.Render())
			return err
		},
	}

	cmd.Flags().StringArrayVar(&extra, "recipient", nil,
		"additional age recipient (age1…) to wrap the DEK to; repeatable")
	return cmd
}

// warnIfPlaintextHostKey prints a loud, actionable warning when the host key
// was left as a bare plaintext file in the data dir — the insecure default a
// stolen data volume defeats (finding E1). The secure tiers say nothing.
func warnIfPlaintextHostKey(prot secrets.HostKeyProtection, warn io.Writer) {
	if prot != secrets.HostKeyPlaintext {
		return
	}
	_, _ = fmt.Fprintln(warn, "")
	_, _ = fmt.Fprintln(warn, "WARNING: host.key is a bare PLAINTEXT file in the data dir.")
	_, _ = fmt.Fprintln(warn, "         A copy of the data volume alone recovers every secret.")
	_, _ = fmt.Fprintf(warn, "         Move it to a secure tier: set %s (or %s) and re-init for a\n",
		secrets.EnvHostKeyPassphrase, secrets.EnvHostKeyPassphraseFile)
	_, _ = fmt.Fprintln(warn, "         passphrase-wrapped key, or install via the Quadlet unit's systemd")
	_, _ = fmt.Fprintln(warn, "         credential (host key unwrapped with a secret outside the volume).")
	_, _ = fmt.Fprintln(warn, "")
}

func newRecoveryKitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recovery-kit",
		Short: "Re-render the recovery kit structure from stored public data",
		Long: "Prints the keyring recipients, their fingerprints, the HKDF salt, and the " +
			"derivation recipe. The recovery secret key is not reprintable: it exists only " +
			"while it is minted, at init or at rotation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir()
			if err != nil {
				return fmt.Errorf("recovery-kit: resolve data dir: %w", err)
			}

			env, err := secrets.Open(dir)
			if err != nil {
				return fmt.Errorf("recovery-kit: %w", err)
			}
			defer func() { _ = env.Close() }()

			kit := env.KitStructure()
			decorateKit(cmd.Context(), kit, cmd.ErrOrStderr())
			_, err = io.WriteString(cmd.OutOrStdout(), kit.Render())
			return err
		},
	}
}
