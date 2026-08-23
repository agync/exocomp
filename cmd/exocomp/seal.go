package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newSealCmd)
}

// newSealCmd manages the optional OpenBao Transit seal for the envelope's DEK
// (ADR 0013). It is the auto-unseal path: with a seal enabled the daemon opens
// the store from OpenBao instead of the on-disk host key. The recovery card
// still opens it without OpenBao — the seal is additive, never a replacement.
func newSealCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seal",
		Short: "Manage the optional OpenBao Transit seal for the envelope (ADR 0013)",
		Long: "Wraps the data-encryption key with an OpenBao Transit key so the daemon " +
			"auto-unseals from OpenBao instead of the on-disk host key. Configure it " +
			"through the environment (" + secrets.EnvSealAddress + ", " + secrets.EnvSealKey +
			", and " + secrets.EnvSealToken + " or " + secrets.EnvSealTokenFile + "); the " +
			"token is never written to the data dir. keyring.age is left intact, so the " +
			"printed recovery card still opens the store with no OpenBao in reach.",
	}
	cmd.AddCommand(newSealEnableCmd(), newSealDisableCmd(), newSealStatusCmd())
	return cmd
}

func newSealEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Wrap the DEK with the configured OpenBao Transit key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSealEnable(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func runSealEnable(ctx context.Context, out io.Writer) (err error) {
	seal, err := secrets.SealConfigFromEnv()
	if err != nil {
		return fmt.Errorf("seal enable: %w", err)
	}
	if !seal.Configured() {
		return fmt.Errorf("seal enable: no seal configured; set %s, %s, and %s (or %s)",
			secrets.EnvSealAddress, secrets.EnvSealKey, secrets.EnvSealToken, secrets.EnvSealTokenFile)
	}
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("seal enable: resolve data dir: %w", err)
	}
	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("seal enable: open envelope (run `exocomp init` first): %w", err)
	}
	defer func() {
		if cerr := env.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if err := secrets.EnableTransitSeal(ctx, env, seal); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "sealed: DEK wrapped by openbao transit key %q; %s written beside keyring.age\n",
		seal.KeyName, secrets.TransitSealName)
	return err
}

func newSealDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Remove the transit seal; revert to opening with the host key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir()
			if err != nil {
				return fmt.Errorf("seal disable: resolve data dir: %w", err)
			}
			if err := secrets.DisableTransitSeal(dir); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"unsealed: %s removed; the daemon opens with the host key\n", secrets.TransitSealName)
			return err
		},
	}
}

func newSealStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "Report the transit seal posture",
		Example: "exocomp seal status --output json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			dir, err := dataDir()
			if err != nil {
				err = fmt.Errorf("seal status: resolve data dir: %w", err)
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return err
			}
			seal, cfgErr := secrets.SealConfigFromEnv()
			present := secrets.TransitSealPresent(dir)

			if mode == outputJSON {
				return emitJSON(cmd, wire.KindSealStatus, sealStatusView(present, seal, cfgErr))
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "transit seal file (%s): %s\n", secrets.TransitSealName, presence(present))
			switch {
			case cfgErr != nil:
				_, _ = fmt.Fprintf(out, "seal env: misconfigured (%v)\n", cfgErr)
			case seal.Configured():
				_, _ = fmt.Fprintf(out, "seal env: configured (key %q at %s)\n", seal.KeyName, seal.Address)
			default:
				_, _ = fmt.Fprintln(out, "seal env: not configured")
			}
			if present && !seal.Configured() && cfgErr == nil {
				_, _ = fmt.Fprintln(out,
					"note: a seal blob exists but no seal env is set; the daemon opens with the host key")
			}
			return nil
		},
	}
}

// sealStatusView projects the same facts the text report prints. The token is
// never read from seal, by construction: SealConfig.Token has no json tag
// path into this view at all.
func sealStatusView(present bool, seal *secrets.SealConfig, cfgErr error) wire.SealStatusView {
	v := wire.SealStatusView{TransitFilePresent: present}
	switch {
	case cfgErr != nil:
		v.EnvError = cfgErr.Error()
	case seal.Configured():
		v.EnvConfigured = true
		v.SealAddress = seal.Address
		v.SealKeyName = seal.KeyName
	}
	if present && !seal.Configured() && cfgErr == nil {
		v.OrphanBlob = true
	}
	return v
}

func presence(b bool) string {
	if b {
		return "present"
	}
	return "absent"
}
