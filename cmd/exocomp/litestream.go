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

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/litestream"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
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
			cfg, err := generateConfigFromStore(cmd.Context(), st, cmd.ErrOrStderr())
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
func generateConfigFromStore(ctx context.Context, st *store.Store, warn io.Writer) (string, error) {
	raw, err := st.GetSetting(ctx, litestream.SettingReplicaTarget)
	if errors.Is(err, store.ErrNotFound) || raw == "" {
		return "", errors.New("no replica target configured — run `exocomp litestream target <uri>` first")
	}
	if err != nil {
		return "", err
	}
	// A replica target may embed s3/sftp credentials (user:pass@host); it is
	// persisted verbatim in the setting table, the same cleartext exposure
	// `backup add-target` warns about at the point of storage.
	if backup.HasURIUserinfo(raw) {
		_, _ = fmt.Fprintf(warn,
			"WARNING: replica target embeds credentials; they are stored in cleartext in the "+
				"database. Prefer sidecar-launch secret injection, an SFTP key, or the "+
				"target's own environment.\n")
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
		Use:     "status",
		Short:   "Report binary presence and replication freshness (RPO age)",
		Example: "exocomp litestream status --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			f, ferr := probeLitestreamLocal(cmd.Context())
			if mode == outputJSON {
				if ferr != nil {
					return failJSON(cmd, ferr)
				}
				return emitJSON(cmd, wire.KindLitestreamStatus, litestreamStatusView(f))
			}
			printLitestreamStatusText(cmd.OutOrStdout(), f)
			return ferr
		},
	}
}

// litestreamFacts is the sidecar's local status probe result — binary
// presence, the persisted target, and replication freshness — captured once
// so the text report and the --output json view derive from identical data
// instead of two probes that could drift apart.
type litestreamFacts struct {
	binPath, version string

	storeErr error

	targetErr error // a real (non-not-found) settings read failure
	noTarget  bool
	target    litestream.Target
	targetRaw string
	parseErr  error

	freshOK    bool
	freshMtime time.Time
	freshErr   error
}

// probeLitestreamLocal gathers litestreamFacts from this box's own store and
// binary presence. The returned error is only a genuine settings-read fault;
// every other absence (no binary, no target, an unparseable target, an
// unreadable freshness probe) is captured as a fact and reported, never
// failed on — the sidecar is optional.
func probeLitestreamLocal(ctx context.Context) (litestreamFacts, error) {
	var f litestreamFacts
	f.binPath, f.version = litestream.Presence()

	st, err := openStore()
	if err != nil {
		f.storeErr = err
		return f, nil
	}
	defer func() { _ = st.Close() }()

	raw, err := st.GetSetting(ctx, litestream.SettingReplicaTarget)
	if errors.Is(err, store.ErrNotFound) || raw == "" {
		f.noTarget = true
		return f, nil
	}
	if err != nil {
		f.targetErr = err
		return f, err
	}
	f.targetRaw = raw
	target, perr := litestream.ParseTarget(raw)
	if perr != nil {
		f.parseErr = perr
		return f, nil
	}
	f.target = target

	_, mtime, ok, ferr := litestream.ReplicaFreshness(target, time.Now().UTC())
	f.freshOK, f.freshMtime, f.freshErr = ok, mtime, ferr
	return f, nil
}

// printLitestreamStatusText renders litestreamFacts exactly as the pre-JSON
// litestreamStatus did — same lines, same order, same silent-return branches
// — so the default text path is unchanged.
func printLitestreamStatusText(w io.Writer, f litestreamFacts) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

	switch {
	case f.binPath == "":
		p("binary:       not installed — %s\n", litestream.InstallHint)
	case f.version == "":
		p("binary:       %s (version probe failed)\n", f.binPath)
	default:
		p("binary:       %s (%s)\n", f.binPath, f.version)
	}

	if f.storeErr != nil {
		p("target:       unknown (%v)\n", f.storeErr)
		return
	}
	if f.noTarget {
		p("target:       none configured — `exocomp litestream target <uri>`\n")
		p("replication:  unknown (no replica target)\n")
		return
	}
	if f.targetErr != nil {
		// Matches the pre-refactor behavior: a real settings-read fault prints
		// nothing further here and fails the command via the returned error.
		return
	}
	if f.parseErr != nil {
		p("target:       %s (UNPARSEABLE: %v)\n", f.targetRaw, f.parseErr)
		return
	}
	p("target:       %s (%s)\n", f.target.Raw, f.target.Type)

	switch {
	case f.freshErr != nil:
		p("replication:  unknown (%v)\n", f.freshErr)
	case f.freshOK:
		age := time.Since(f.freshMtime).Truncate(time.Second)
		p("replication:  last frame %s ago (RPO ~%s), replica %s\n", age, age, f.freshMtime.UTC().Format(time.RFC3339))
	case f.target.Remote():
		p("replication:  unknown — a remote replica's frame age needs the litestream binary to read\n")
	default:
		p("replication:  no replicated frames yet — sidecar not running?\n")
	}
}

// litestreamStatusView projects litestreamFacts into the wire view. The
// target's raw URI never travels (wire.LitestreamStatusView's doc explains
// why); everything else mirrors the branch printLitestreamStatusText took.
func litestreamStatusView(f litestreamFacts) wire.LitestreamStatusView {
	v := wire.LitestreamStatusView{
		BinaryPresent: f.binPath != "",
	}
	if f.version != "" {
		v.BinaryVersion = f.version
	}

	switch {
	case f.storeErr != nil, f.targetErr != nil:
		v.ReplicationState = wire.LitestreamReplicationUnknown
	case f.noTarget:
		v.ReplicationState = wire.LitestreamReplicationNotConfigured
	case f.parseErr != nil:
		v.ReplicationState = wire.LitestreamReplicationUnparseable
	default:
		v.TargetConfigured = true
		v.TargetType = string(f.target.Type)
		v.TargetRemote = f.target.Remote()
		switch {
		case f.freshErr != nil:
			v.ReplicationState = wire.LitestreamReplicationProbeFailed
		case f.freshOK:
			v.ReplicationState = wire.LitestreamReplicationFresh
			v.ReplicaFreshAt = f.freshMtime.UTC().Format(time.RFC3339)
		case f.target.Remote():
			v.ReplicationState = wire.LitestreamReplicationRemoteUnknown
		default:
			v.ReplicationState = wire.LitestreamReplicationNoFrames
		}
	}
	return v
}
