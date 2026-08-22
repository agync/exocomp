package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
)

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
