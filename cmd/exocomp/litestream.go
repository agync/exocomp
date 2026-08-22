package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/litestream"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newLitestreamCmd)
}

func newLitestreamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "litestream",
		Short: "Manage the optional Litestream WAL-replication sidecar",
		Long: "Litestream replicates exocomp.db's WAL to a replica for seconds-level RPO,\n" +
			"as a fast-RPO complement to the authoritative restic DR chain (ADR 0012).\n" +
			"It runs beside exocompd as a sidecar; exocomp generates its config and\n" +
			"observes the replica, it does not run litestream inside the daemon.",
	}
	cmd.AddCommand(newLitestreamTargetCmd(), newLitestreamConfigCmd(), newLitestreamStatusCmd())
	return cmd
}

// dbPath is the path to exocomp.db, the database Litestream replicates.
func litestreamDBPath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "exocomp.db"), nil
}

func newLitestreamTargetCmd() *cobra.Command {
	var ack bool
	cmd := &cobra.Command{
		Use:   "target <uri>",
		Short: "Persist the replica target (a path, file://, s3://, or sftp://)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := litestream.ParseTarget(args[0])
			if err != nil {
				return err
			}
			// Enforce the no-encryption caveat at the point of persistence, so a
			// later `config`/sidecar launch cannot ship to a plaintext remote the
			// operator never acknowledged.
			if target.Remote() && !ack {
				return fmt.Errorf("%w", litestream.ErrUnencryptedRemote)
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err := st.PutSetting(cmd.Context(), litestream.SettingReplicaTarget, target.Raw); err != nil {
				return err
			}
			ackValue := "false"
			if ack {
				ackValue = "true"
			}
			if err := st.PutSetting(cmd.Context(), litestream.SettingReplicaAck, ackValue); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(w, "replica target set: %s (%s)\n", target.Raw, target.Type)
			if target.Remote() {
				_, _ = fmt.Fprintln(w, "note: Litestream does not encrypt — this remote target must be an")
				_, _ = fmt.Fprintln(w, "      already-encrypted store or trusted transport (acknowledged).")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&ack, "ack-unencrypted", false, "acknowledge that a remote target is not encrypted by Litestream")
	return cmd
}

func newLitestreamConfigCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Emit the generated litestream.yml for the configured target",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			cfg, err := generateConfigFromStore(cmd.Context(), st)
			if err != nil {
				return err
			}
			if out == "" {
				_, err := io.WriteString(cmd.OutOrStdout(), cfg)
				return err
			}
			if err := os.WriteFile(out, []byte(cfg), 0o600); err != nil {
				return fmt.Errorf("write config: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write to this path instead of stdout")
	return cmd
}

// generateConfigFromStore assembles a Config from the persisted target and the
// live DB path, then renders it. The ack is read back so a target persisted
// with --ack-unencrypted keeps rendering without re-prompting.
func generateConfigFromStore(ctx context.Context, st *store.Store) (string, error) {
	raw, err := st.GetSetting(ctx, litestream.SettingReplicaTarget)
	if errors.Is(err, store.ErrNotFound) || raw == "" {
		return "", errors.New("no replica target configured — run `exocomp litestream target <uri>` first")
	}
	if err != nil {
		return "", err
	}
	target, err := litestream.ParseTarget(raw)
	if err != nil {
		return "", err
	}
	ack := false
	if v, err := st.GetSetting(ctx, litestream.SettingReplicaAck); err == nil {
		ack = v == "true"
	}
	dbPath, err := litestreamDBPath()
	if err != nil {
		return "", err
	}
	return litestream.GenerateConfig(litestream.Config{DBPath: dbPath, Target: target, AckUnencrypted: ack})
}

func newLitestreamStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report binary presence and replication freshness (RPO age)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return litestreamStatus(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

// litestreamStatus prints the sidecar's honest state: whether the binary is
// installed, what target is configured, and how fresh the replica is. It never
// fails for want of the binary — an absent sidecar is a state to report, with
// the one install step named, not an error.
func litestreamStatus(ctx context.Context, w io.Writer) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

	binPath, version := litestream.Presence()
	switch {
	case binPath == "":
		p("binary:       not installed — %s\n", litestream.InstallHint)
	case version == "":
		p("binary:       %s (version probe failed)\n", binPath)
	default:
		p("binary:       %s (%s)\n", binPath, version)
	}

	st, err := openStore()
	if err != nil {
		p("target:       unknown (%v)\n", err)
		return nil
	}
	defer func() { _ = st.Close() }()

	raw, err := st.GetSetting(ctx, litestream.SettingReplicaTarget)
	if errors.Is(err, store.ErrNotFound) || raw == "" {
		p("target:       none configured — `exocomp litestream target <uri>`\n")
		p("replication:  unknown (no replica target)\n")
		return nil
	}
	if err != nil {
		return err
	}
	target, perr := litestream.ParseTarget(raw)
	if perr != nil {
		p("target:       %s (UNPARSEABLE: %v)\n", raw, perr)
		return nil
	}
	p("target:       %s (%s)\n", target.Raw, target.Type)

	_, mtime, ok, ferr := litestream.ReplicaFreshness(target, time.Now().UTC())
	if ferr != nil {
		p("replication:  unknown (%v)\n", ferr)
		return nil
	}
	switch {
	case ok:
		age := time.Since(mtime).Truncate(time.Second)
		p("replication:  last frame %s ago (RPO ~%s), replica %s\n", age, age, mtime.UTC().Format(time.RFC3339))
	case target.Remote():
		p("replication:  unknown — a remote replica's frame age needs the litestream binary to read\n")
	default:
		p("replication:  no replicated frames yet — sidecar not running?\n")
	}
	return nil
}
