package main

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func newBackupDrillCmd() *cobra.Command {
	var only string

	cmd := &cobra.Command{
		Use:   "drill",
		Short: "Restore the newest snapshot into a throwaway dir and verify it",
		Long: "Runs the same restore, digest verify, and PRAGMA integrity_check a real " +
			"restore would, into a directory it creates and destroys itself; the live " +
			"data dir is never touched. The restic password comes straight from the " +
			"running envelope — the same derivation a backup writes with — so no age " +
			"identity is needed and this can run on a cadence (`exocomp schedule add " +
			"--kind drill`). A git target needs one anyway: a git leg is only ever " +
			"encrypted to public recipients, so drill it manually with `exocomp restore " +
			"--identity` instead.\n\n" +
			"OK means the snapshot restored, its manifest digests verified, and the " +
			"restored database opened and passed integrity_check. That is a drill " +
			"finding when it does not hold (a bad backup) — reported and exited " +
			"nonzero. A target the drill could not even reach (unresolved, no backup " +
			"yet) is a plain error.",
		Example: "exocomp backup drill --output json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackupDrill(cmd, only)
		},
	}
	cmd.Flags().StringVar(&only, "target", "", "drill this target only")
	return cmd
}

// drillOutcome is one target's attempt: either result carries what the drill
// found, or err carries why it could not run at all (backup.RestoreDrill's
// own finding-vs-infra-error split, internal/backup/drill.go).
type drillOutcome struct {
	target store.BackupTarget
	result backup.DrillResult
	err    error
}

func runBackupDrill(cmd *cobra.Command, only string) error {
	mode, err := outputMode(cmd)
	if err != nil {
		return err
	}
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("backup drill: resolve data dir: %w", err)
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListBackupTargets(cmd.Context(), false)
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
		return errors.New("backup drill: no enabled targets")
	}

	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("backup drill: %w", err)
	}
	defer func() { _ = env.Close() }()

	outcomes := make([]drillOutcome, 0, len(targets))
	for _, t := range targets {
		outcomes = append(outcomes, drillTarget(cmd, env, t))
	}

	if mode == outputJSON {
		views := make([]wire.DrillView, 0, len(outcomes))
		for _, oc := range outcomes {
			views = append(views, drillView(oc))
		}
		if err := emitJSON(cmd, wire.KindBackupDrill, views); err != nil {
			return err
		}
		return drillOutcomeErr(outcomes)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TARGET\tSTATE\tSNAPSHOT\tRPO\tDETAIL")
	for _, oc := range outcomes {
		writeDrillRow(w, oc)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return drillOutcomeErr(outcomes)
}

// drillTarget runs one target's drill. A git target is refused up front by
// LiveSource (with a message pointing at the manual --identity path), which
// this reports as an infra error like any other target the drill could not
// reach — it never got as far as inspecting a snapshot.
func drillTarget(cmd *cobra.Command, env *secrets.Envelope, t store.BackupTarget) drillOutcome {
	oc := drillOutcome{target: t}
	src, err := backup.LiveSource(env, t, targetEnv)
	if err != nil {
		oc.err = err
		return oc
	}
	result, err := backup.RestoreDrill(cmd.Context(), src, "")
	if err != nil {
		oc.err = err
		return oc
	}
	oc.result = result
	return oc
}

func drillView(oc drillOutcome) wire.DrillView {
	if oc.err != nil {
		return wire.NewDrillErrorView(oc.target.Name, oc.target.Kind+":"+oc.target.URI, oc.err)
	}
	return wire.NewDrillView(oc.target.Name, oc.result)
}

func writeDrillRow(w *tabwriter.Writer, oc drillOutcome) {
	if oc.err != nil {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", oc.target.Name, "error", "-", "-", oc.err.Error())
		return
	}
	state, detail := "OK", fmt.Sprintf("%d file(s), %d bytes verified", oc.result.VerifiedFiles, oc.result.VerifiedBytes)
	if !oc.result.OK {
		state, detail = "FAIL", oc.result.Reason
	}
	rpo := "-"
	if oc.result.SnapshotID != "" {
		rpo = oc.result.RPOAge.Round(time.Second).String()
	}
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
		oc.target.Name, state, shortID(oc.result.SnapshotID), rpo, detail)
}

// drillOutcomeErr joins every target's problem — an infra error or a drill
// finding alike — into the one error that decides the process exit code
// (exitCodeFor -> wire.ExitFor), the same pattern runBackup uses for a failed
// leg: no sentinel matches either kind, so both settle the generic-failure
// exit code, which is what makes "a run settled failed" (wire.ExitFailure's
// own description) the right, already-picked code for a drill finding too.
func drillOutcomeErr(outcomes []drillOutcome) error {
	var errs []error
	for _, oc := range outcomes {
		switch {
		case oc.err != nil:
			errs = append(errs, fmt.Errorf("target %s: %w", oc.target.Name, oc.err))
		case !oc.result.OK:
			errs = append(errs, fmt.Errorf("target %s: %s", oc.target.Name, oc.result.Reason))
		}
	}
	return errors.Join(errs...)
}
