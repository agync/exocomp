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
		from                 string
		dest                 string
		identity             string
		name                 string
		snapshot             string
		canary               string
		allowUnauthenticated bool
		targetAuthenticates  bool
		fromLitestream       bool
		replica              string
		replicaEncrypted     bool
	)

	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Rebuild a data dir from the newest snapshot on a target",
		Long: "Fetches keyring.age from the target root, unwraps the data encryption key " +
			"with the recovery identity, derives the repository password, restores the " +
			"newest snapshot (or the one --snapshot names), and verifies every sha256 in " +
			"its manifest. It refuses a destination that already holds a keyring.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The replica leg restores the database only and returns before
			// the restic source is parsed, so a --snapshot selector handed
			// alongside it would be silently dropped — refuse it instead
			// (a silently ignored selector is the pretend this round kills).
			if fromLitestream && snapshot != "" {
				return fmt.Errorf("restore: --snapshot cannot be combined with --from-litestream (the replica leg restores the database only)")
			}
			// The fast-RPO path recovers exocomp.db ONLY, from the Litestream
			// replica, onto a data dir that already holds the keyring. It is a
			// different chain from the authoritative restic restore below, so it
			// branches before the restic source is even parsed.
			if fromLitestream {
				return runLitestreamRestore(cmd, litestreamRestoreArgs{
					dest:             dest,
					identity:         identity,
					replica:          replica,
					replicaEncrypted: replicaEncrypted,
				})
			}

			if from == "" {
				return fmt.Errorf("restore: --from is required (or use --from-litestream)")
			}
			src, err := backup.ParseSource(from)
			if err != nil {
				return err
			}
			src.Name = name
			src.Snapshot = snapshot
			src.IdentityFile = identity
			src.Env = restoreEnv(src.Kind)
			src.AllowUnauthenticated = allowUnauthenticated
			// From the recovery card (section 6): a target that authenticates its
			// snapshots must not be silently downgraded when an attacker strips
			// the in-repo tag + marker — that stripped snapshot is refused as a
			// downgrade, not trusted as a never-authenticated legacy target (B1).
			src.Authenticates = targetAuthenticates
			// An unauthenticated snapshot restores, but never quietly: the
			// operator reading this is deciding whether to trust the result.
			src.Warn = func(msg string) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: "+msg)
			}

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
	cmd.Flags().StringVar(&snapshot, "snapshot", "",
		"restic snapshot id (full or unambiguous prefix) to restore; empty restores the newest")
	cmd.Flags().StringVar(&canary, "canary", "", "after restoring, open this sealed secret to prove the chain")
	cmd.Flags().BoolVar(&targetAuthenticates, "target-authenticates", false,
		"assert (from the recovery card, section 6) that this target authenticates its "+
			"snapshots, so a snapshot whose tag+marker were stripped is refused as a downgrade (B1)")
	cmd.Flags().BoolVar(&allowUnauthenticated, "allow-unauthenticated", false,
		"accept a snapshot with no manifest tag from a target that authenticates its snapshots "+
			"(warns instead of refusing); only for a snapshot you are certain is yours")
	cmd.Flags().BoolVar(&fromLitestream, "from-litestream", false,
		"fast-RPO path: restore exocomp.db ONLY from the Litestream replica, onto a data dir "+
			"that already holds the keyring (run the full restic restore first); schedules stay paused")
	cmd.Flags().StringVar(&replica, "replica", "",
		"Litestream replica url; read from the "+backup.SettingReplicaURL+" setting when empty")
	cmd.Flags().BoolVar(&replicaEncrypted, "i-know-the-replica-is-encrypted", false,
		"acknowledge a plaintext public-remote replica — Litestream does not encrypt, so this "+
			"asserts the store itself is encrypted; a LAN-local or file replica needs no ack")
	// --from is required for the restic restore but meaningless for the
	// litestream path, so it is validated in RunE, not marked globally required.
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
