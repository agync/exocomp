package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/backup"
)

func init() {
	registerCommand(newRestoreCmd)
}

func newRestoreCmd() *cobra.Command {
	var (
		from     string
		dest     string
		identity string
		name     string
		canary   string
	)

	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Rebuild a data dir from the newest snapshot on a target",
		Long: "Fetches keyring.age from the target root, unwraps the data encryption key " +
			"with the recovery identity, derives the repository password, restores the " +
			"newest snapshot, and verifies every sha256 in its manifest. It refuses a " +
			"destination that already holds a keyring.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, err := backup.ParseSource(from)
			if err != nil {
				return err
			}
			src.Name = name
			src.IdentityFile = identity
			src.Env = restoreEnv(src.Kind)

			manifest, err := backup.RestoreLatest(cmd.Context(), src, dest)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "restored %s from %s:%s\n", dest, src.Kind, src.URI)
			_, _ = fmt.Fprintf(out, "  taken:  %s\n", manifest.CreatedAt.Format("2006-01-02 15:04:05Z"))
			_, _ = fmt.Fprintf(out, "  schema: %d (exocomp %s)\n", manifest.SchemaVersion, manifest.ExocompVersion)
			for _, f := range manifest.Files {
				_, _ = fmt.Fprintf(out, "  ok:     %s  %d bytes  sha256 %s\n", f.Name, f.Size, f.SHA256[:16])
			}

			if canary == "" {
				return nil
			}
			n, err := backup.VerifyCanary(cmd.Context(), dest, identity, canary)
			if err != nil {
				return fmt.Errorf("restore: canary %q: %w", canary, err)
			}
			// The plaintext stays out of stdout: a canary is still a secret.
			_, _ = fmt.Fprintf(out, "  canary: %s opened, %d bytes\n", canary, n)
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "target uri, or kind:uri (restic_local|restic_s3|git)")
	cmd.Flags().StringVar(&dest, "dest", "", "data dir to rebuild; must not already hold a keyring")
	cmd.Flags().StringVar(&identity, "identity", "", "age identity file holding the recovery key")
	cmd.Flags().StringVar(&name, "target-name", "",
		"target name the repository password was derived from; read from RECOVERY.md when empty")
	cmd.Flags().StringVar(&canary, "canary", "", "after restoring, open this sealed secret to prove the chain")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("dest")
	return cmd
}

func restoreEnv(kind string) map[string]string {
	if kind != backup.KindResticS3 {
		return nil
	}
	env := map[string]string{}
	for _, k := range s3EnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	return env
}
