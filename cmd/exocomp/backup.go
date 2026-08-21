package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// s3EnvKeys are the variables a restic_s3 target needs. They are read from the
// process environment for now; they become sealed secret rows bound to the
// target once the secret store has a CLI surface.
var s3EnvKeys = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_DEFAULT_REGION",
	"AWS_REGION",
}

func init() {
	registerCommand(newBackupCmd)
}

func newBackupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Assemble a snapshot and fan it out to every enabled target",
	}
	cmd.AddCommand(newBackupAddTargetCmd(), newBackupListCmd(), newBackupRunCmd(), newBackupCanaryCmd())
	return cmd
}

func newBackupAddTargetCmd() *cobra.Command {
	var t store.BackupTarget
	var disabled bool

	cmd := &cobra.Command{
		Use:   "add-target",
		Short: "Register a backup target",
		Long: "Kinds: restic_local (uri is a directory), restic_s3 (uri is restic's " +
			"s3:<endpoint>/<bucket> form), git (uri is a bare repository). Retention is " +
			"\"daily=7,weekly=4\" style, empty for the default policy, or \"none\".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch t.Kind {
			case backup.KindResticLocal, backup.KindResticS3, backup.KindGit:
			default:
				return fmt.Errorf("backup add-target: kind must be one of %s, %s, %s",
					backup.KindResticLocal, backup.KindResticS3, backup.KindGit)
			}
			t.Enabled = !disabled

			// The uri is stored in plain SQLite and handed to subprocesses.
			// A token embedded in it is therefore at rest in the database and
			// one echoed line away from a job's recorded output.
			if backup.HasURIUserinfo(t.URI) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"WARNING: --uri embeds credentials; they are stored in cleartext in the "+
						"database. Prefer an ssh key, a credential helper, or the target's "+
						"own environment.\n")
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			created, err := st.CreateBackupTarget(cmd.Context(), t)
			if err != nil {
				if errors.Is(err, store.ErrDuplicate) {
					return fmt.Errorf("backup add-target: target %q already exists", t.Name)
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "target %s (%s) added as %s\n",
				created.Name, created.Kind, enabledLabel(created.Enabled))
			return err
		},
	}
	cmd.Flags().StringVar(&t.Name, "name", "", "target name; the restic password is derived from it")
	cmd.Flags().StringVar(&t.Kind, "kind", "", "restic_local|restic_s3|git")
	cmd.Flags().StringVar(&t.URI, "uri", "", "repository location")
	cmd.Flags().StringVar(&t.Retention, "retention", "", "e.g. daily=7,weekly=4; empty for the default")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "register the target without enabling it")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("kind")
	_ = cmd.MarkFlagRequired("uri")
	return cmd
}

func newBackupListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List targets and their last run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			targets, err := st.ListBackupTargets(cmd.Context(), true)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tKIND\tURI\tRETENTION\tSTATE\tLAST RUN")
			for _, t := range targets {
				runs, err := st.ListBackupRuns(cmd.Context(), t.ID, 1)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					t.Name, t.Kind, t.URI, retentionLabel(t.Retention),
					enabledLabel(t.Enabled), lastRunLabel(runs))
			}
			return w.Flush()
		},
	}
}

func newBackupRunCmd() *cobra.Command {
	var only string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Back up to every enabled target",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackup(cmd.Context(), cmd.OutOrStdout(), only)
		},
	}
	cmd.Flags().StringVar(&only, "target", "", "back up to this target only")
	return cmd
}

func runBackup(ctx context.Context, w io.Writer, only string) (err error) {
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("backup run: resolve data dir: %w", err)
	}
	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("backup run: %w", err)
	}
	defer func() { _ = env.Close() }()

	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	rows, err := st.ListBackupTargets(ctx, false)
	if err != nil {
		return err
	}
	var targets []backup.Target
	for _, row := range rows {
		if only != "" && row.Name != only {
			continue
		}
		targets = append(targets, backup.Target{BackupTarget: row, Env: targetEnv(row)})
	}
	if len(targets) == 0 {
		return errors.New("backup run: no enabled targets")
	}

	report, err := backup.New(st, env, backup.Options{}).Run(ctx, targets)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TARGET\tKIND\tSTATE\tSNAPSHOT\tADDED\tDETAIL")
	for _, leg := range report.Legs {
		detail := ""
		if leg.Err != nil {
			detail = leg.Err.Error()
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			leg.Target, leg.Kind, leg.State, shortID(leg.SnapshotID), leg.BytesAdded, detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return report.Err()
}

// newBackupCanaryCmd seals a canary secret so a restore can be proven end to
// end before the secret store has its own CLI. The value is read from stdin,
// never from argv.
func newBackupCanaryCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "canary <name>",
		Short:  "Seal a canary secret read from stdin",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<16))
			if err != nil {
				return fmt.Errorf("backup canary: read value: %w", err)
			}
			if len(value) == 0 {
				return errors.New("backup canary: empty value on stdin")
			}

			dir, err := dataDir()
			if err != nil {
				return fmt.Errorf("backup canary: resolve data dir: %w", err)
			}
			env, err := secrets.Open(dir)
			if err != nil {
				return fmt.Errorf("backup canary: %w", err)
			}
			defer func() { _ = env.Close() }()

			sealed, err := env.Seal(args[0], "canary", value)
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			_, err = st.CreateSecret(cmd.Context(), store.SecretRow{
				Name:       sealed.Name,
				Kind:       sealed.Kind,
				Nonce:      sealed.Nonce,
				Ciphertext: sealed.Ciphertext,
				AAD:        sealed.Name + "|" + sealed.Kind,
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "sealed canary %q (%d bytes)\n", sealed.Name, len(value))
			return err
		},
	}
}

func openStore() (*store.Store, error) {
	dir, err := dataDir()
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return store.Open(filepath.Join(dir, "exocomp.db"))
}

func targetEnv(t store.BackupTarget) map[string]string {
	if t.Kind != backup.KindResticS3 {
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

func enabledLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func retentionLabel(r string) string {
	if r == "" {
		return "default"
	}
	return r
}

func lastRunLabel(runs []store.BackupRun) string {
	if len(runs) == 0 {
		return "never"
	}
	return runs[0].State + " " + runs[0].StartedAt.Format("2006-01-02 15:04:05Z")
}

func shortID(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
