package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
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
	cmd.AddCommand(newBackupAddTargetCmd(), newBackupListCmd(), newBackupRunCmd(), newBackupDrillCmd())
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
		Use:     "list",
		Short:   "List targets and their last run",
		Example: "exocomp backup list --output json",
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

			targets, err := st.ListBackupTargets(cmd.Context(), true)
			if err != nil {
				return err
			}
			if mode == outputJSON {
				// wire.BackupTargetView carries the URI verbatim; scrub it here
				// rather than growing the projection a redaction-aware branch.
				scrubbed := make([]store.BackupTarget, len(targets))
				copy(scrubbed, targets)
				for i := range scrubbed {
					scrubbed[i].URI = backup.ScrubURIUserinfo(scrubbed[i].URI)
				}
				return emitJSON(cmd, wire.KindBackupList, wire.NewBackupTargetList(scrubbed))
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tKIND\tURI\tRETENTION\tSTATE\tLAST RUN")
			for _, t := range targets {
				runs, err := st.ListBackupRuns(cmd.Context(), t.ID, 1)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					t.Name, t.Kind, backup.ScrubURIUserinfo(t.URI), retentionLabel(t.Retention),
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

// runBackup enqueues one backup job per matching target — kind backup,
// concurrency key "backup:<target>" — closing the carve-out recorded in
// docs/architecture/overview.md: a backup runs through the same queue as
// everything else, not as a direct engine call. A running `exocomp serve`
// claims and runs those jobs while this command follows them; with no daemon
// up the queue is drained here instead.
func runBackup(ctx context.Context, w io.Writer, only string) (err error) {
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("backup run: resolve data dir: %w", err)
	}

	st, err := openJobStore(ctx, w)
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
	var targets []store.BackupTarget
	for _, row := range rows {
		if only != "" && row.Name != only {
			continue
		}
		targets = append(targets, row)
	}
	if len(targets) == 0 {
		return errors.New("backup run: no enabled targets")
	}

	specs := make([]queue.Spec, 0, len(targets))
	for _, t := range targets {
		specs = append(specs, queue.Spec{
			Runbook:        "backup",
			Kind:           backup.JobKind,
			Trigger:        "manual",
			Argv:           backup.Argv(t.Name),
			ConcurrencyKey: backup.ConcurrencyKey(t.Name),
		})
	}

	if daemonRunning(ctx, dir) {
		if _, err := enqueueAllForDaemon(ctx, st, dir, specs, w); err != nil {
			return err
		}
	} else if err := runBackupInline(ctx, st, dir, specs); err != nil {
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TARGET\tKIND\tSTATE\tSNAPSHOT\tADDED\tDETAIL")
	var failed []error
	for _, t := range targets {
		run, runErr := latestBackupRun(ctx, st, t.ID)
		if runErr != nil {
			failed = append(failed, fmt.Errorf("target %s: %w", t.Name, runErr))
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", t.Name, t.Kind, "unknown", "-", 0, runErr.Error())
			continue
		}
		if run.State != store.BackupSucceeded {
			failed = append(failed, fmt.Errorf("target %s: %s", t.Name, run.Error))
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			t.Name, t.Kind, run.State, shortID(run.SnapshotID), run.BytesAdded, run.Error)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return errors.Join(failed...)
}

// runBackupInline drains the jobs in this process, which is what a host with
// no daemon does. The envelope is opened here and never leaves: a backup job
// row alone is not enough to run one.
func runBackupInline(ctx context.Context, st *store.Store, dir string, specs []queue.Spec) error {
	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("backup run: %w", err)
	}
	defer func() { _ = env.Close() }()

	quiesce, err := backup.LoadQuiesce(filepath.Join(dir, ".exocomp", "backup-quiesce.yml"))
	if err != nil {
		return fmt.Errorf("backup run: %w", err)
	}
	runner := backup.NewRunner(st, env, backup.Options{Quiesce: quiesce}, targetEnv)
	q := queue.New(st, runner, queue.Options{Workers: len(specs), PollInterval: 50 * time.Millisecond})
	for _, spec := range specs {
		if _, err := q.Enqueue(ctx, spec); err != nil {
			return err
		}
	}
	return q.Drain(ctx)
}

// latestBackupRun returns a target's most recent backup_run row, the one the
// job just enqueued created.
func latestBackupRun(ctx context.Context, st *store.Store, targetID int64) (store.BackupRun, error) {
	runs, err := st.ListBackupRuns(ctx, targetID, 1)
	if err != nil {
		return store.BackupRun{}, err
	}
	if len(runs) == 0 {
		return store.BackupRun{}, errors.New("no backup_run row")
	}
	return runs[0], nil
}

func openStore() (*store.Store, error) {
	dir, err := dataDir()
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	return openStoreAt(dir)
}

// openStoreAt opens the store under an explicit data dir, for callers that hold
// the directory directly rather than resolving it from the environment (the
// setup wizard, which writes into the dir it is provisioning). It is not a job
// path and deliberately does not sweep; it lives here so the store.Open call
// that the sweep-wiring guard polices stays in this one file.
func openStoreAt(dir string) (*store.Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return store.Open(filepath.Join(dir, "exocomp.db"))
}

// openJobStore opens the store for a command that runs jobs, first reconciling
// every non-terminal row a crash left stranded: rows a dead process left queued
// or running, and gated rows whose approval decision was interrupted. Every such
// command reconciles, not just the first one anybody wrote: a job left running by
// a killed process holds its concurrency key forever, and nothing else in the
// system will ever move it.
func openJobStore(ctx context.Context, warn io.Writer) (*store.Store, error) {
	st, err := openStore()
	if err != nil {
		return nil, err
	}
	rec, err := st.ReconcileNonTerminal(ctx)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if warn != nil {
		if gone := rec.Interrupted + rec.Orphaned; gone > 0 {
			_, _ = fmt.Fprintf(warn, "swept %d job(s) left by a process that is gone to interrupted\n", gone)
		}
		if settled := rec.Released + rec.Cancelled; settled > 0 {
			_, _ = fmt.Fprintf(warn, "reconciled %d job(s) whose approval decision was interrupted\n", settled)
		}
	}
	return st, nil
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
