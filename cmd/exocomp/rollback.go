package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/store"
)

// preRollbackSubdir holds the pre-rollback safety snapshots, one timestamped
// dir each, under the upgrade root. They are the escape hatch's own escape
// hatch: the current database is copied here before a rollback overwrites it,
// so a rollback run by mistake can itself be undone.
const preRollbackSubdir = "pre-rollback"

func init() {
	registerCommand(newRollbackCmd)
}

func newRollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback",
		Short: "Restore the most recent pre-upgrade binary and data snapshot on demand",
		Long: "The manual escape hatch for a bad upgrade: it finds the most recent upgrade's " +
			"rollback point under the data dir, restores the preserved prior binary (if that " +
			"upgrade swapped one in) and the pre-upgrade data snapshot, and reports the ledger of " +
			"the upgrade it reversed. Like upgrade, it never runs sudo and touches only the data " +
			"dir and the binary's own directory. Restart the service afterwards to run on the " +
			"restored binary.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRollback(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func runRollback(ctx context.Context, out io.Writer) error {
	live, err := resolveSelf()
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	return rollbackWithBinary(ctx, out, live)
}

// rollbackWithBinary is the rollback with the live binary already resolved, so
// a test can drive it against a stand-in binary rather than the test process.
func rollbackWithBinary(ctx context.Context, out io.Writer, live string) error {
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("rollback: resolve data dir: %w", err)
	}

	// A running daemon would be draining the queue and holding the binary live;
	// restoring the file and database under it is not safe. Stopping it is the
	// operator's job, the same as for upgrade.
	if daemonRunning(ctx, dir) {
		return errors.New("rollback: a daemon is serving this data dir; stop it first, then re-run rollback")
	}

	upgradeRoot := filepath.Join(dir, upgradeSubdir)
	workflowID, workflowDir, err := latestUpgradeDir(upgradeRoot)
	if err != nil {
		if errors.Is(err, errNoUpgradePoint) {
			return fmt.Errorf("rollback: no upgrade rollback point found under %s; nothing to roll back to", upgradeRoot)
		}
		return err
	}
	_ = workflowDir // paths are reconstructed from the workflow id below

	paths := upgradePathsFor(dir, live, "", workflowID)
	_, _ = fmt.Fprintf(out, "rollback: reversing upgrade %s\n", workflowID)

	// Show the ledger of the upgrade being reversed, if the store still holds it
	// — the choreography, as steps, is the record of what is being undone.
	printUpgradeLedger(ctx, out, dir, workflowID)

	// Spell out what the restore throws away: the pre-upgrade snapshot replaces
	// the live database wholesale, so every DB write since the upgrade is lost.
	warnRollbackDiscards(out, workflowID)

	// Snapshot the current database before overwriting it, so a rollback run by
	// mistake is itself reversible. Best-effort and loud: the escape hatch must
	// still work when the database it would snapshot is the very thing that is
	// broken, so a snapshot failure warns but does not block the rollback.
	preRollbackSafetySnapshot(ctx, dir, out)

	if err := restorePriorState(ctx, paths, workflowID, out); err != nil {
		return fmt.Errorf("rollback: restore hit an error: %w — inspect %s by hand", err, paths.workflowDir)
	}
	_, _ = fmt.Fprintf(out, "rollback complete; restart the service to run on the restored binary "+
		"(e.g. systemctl --user restart exocomp)\n")
	return nil
}

// printUpgradeLedger prints the reversed upgrade's steps when the store still
// has them. A missing ledger is not an error: the filesystem rollback point,
// not the database row, is what a rollback depends on.
func printUpgradeLedger(ctx context.Context, out io.Writer, dir, workflowID string) {
	st, err := openStoreAt(dir)
	if err != nil {
		return
	}
	defer func() { _ = st.Close() }()

	steps, err := st.GetWorkflow(ctx, workflowID)
	if err != nil || len(steps) == 0 {
		return
	}
	printLedger(out, steps)
	_, _ = fmt.Fprintf(out, "(that upgrade's status was %s)\n", store.WorkflowStatus(steps))
}

// warnRollbackDiscards names, in plain terms, what a rollback throws away: the
// restore puts back the database as it was at the START of the named upgrade,
// so everything written to it since is gone. The list is by category, not an
// exhaustive table dump — enough for an operator to know the blast radius
// before the irreversible step runs.
func warnRollbackDiscards(out io.Writer, workflowID string) {
	_, _ = fmt.Fprintf(out, "\nWARNING: rollback restores the database snapshot taken at the start of "+
		"upgrade %s.\nEverything written to the database SINCE that upgrade is DISCARDED, including:\n"+
		"  - jobs run and their event history\n"+
		"  - backups recorded, workspaces and their tofu state rows\n"+
		"  - secrets sealed or changed, approvals and schedule changes\n"+
		"The binary is also reverted to the pre-upgrade one.\n", workflowID)
}

// preRollbackSafetySnapshot copies the current database into a fresh timestamped
// dir under the upgrade root before the rollback overwrites it. LocalRestore
// only replaces the restored database file, so this snapshot (living elsewhere
// under the data dir) survives the rollback and can be restored by hand if the
// rollback was a mistake. A failure is reported but does not abort: rollback is
// the recovery path and must not be blocked by a failed safety net.
func preRollbackSafetySnapshot(ctx context.Context, dir string, out io.Writer) {
	st, err := openStoreAt(dir)
	if err != nil {
		_, _ = fmt.Fprintf(out, "WARNING: pre-rollback safety snapshot skipped: open database: %v\n", err)
		return
	}
	dest := filepath.Join(dir, upgradeSubdir, preRollbackSubdir,
		time.Now().UTC().Format("20060102T150405.000000000Z"))
	m, err := backup.LocalSnapshot(ctx, st, dest)
	_ = st.Close()
	if err != nil {
		_, _ = fmt.Fprintf(out, "WARNING: pre-rollback safety snapshot FAILED (%v); "+
			"the rollback will proceed but will NOT be reversible\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "safety: current database snapshotted to %s (schema %d) before rollback;\n"+
		"restore it by hand if this rollback was a mistake\n", dest, m.SchemaVersion)
}
