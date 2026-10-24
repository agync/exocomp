package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newReloadCmd)
}

func newReloadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Reload the daemon's schedules from the database",
		Long: "Rebuilds a running daemon's schedule registrations from the database — the " +
			"same path SIGHUP and --reload-interval drive — so a schedule added, edited, " +
			"enabled, or disabled through the CLI takes effect without a restart. " +
			"A reload that cannot read the database leaves the running set alone.\n\n" +
			"reload controls a RUNNING daemon over the operator API, so it needs a selected " +
			"remote (--remote, EXOCOMP_REMOTE, or the hosts.toml default) — the control node targets its own daemon.",
		Example: "exocomp --remote self reload",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReload(cmd)
		},
	}
	return cmd
}

// runReload asks the selected remote daemon to rebuild its schedules and
// reports how many enabled schedules the store holds (an upper bound on the
// live set: a row the cron parser rejects is left out of the registrations).
// Schedule control is live daemon state, so it always goes over the API —
// there is no local in-process daemon a separate CLI could poke, and a signal
// could not return the count the answer carries.
func runReload(cmd *cobra.Command) error {
	mode, err := outputMode(cmd)
	if err != nil {
		return err
	}

	rc, remote, err := remoteClient(cmd)
	if err != nil {
		return err
	}
	if !remote {
		return errors.New("reload controls a running daemon; select it with --remote, EXOCOMP_REMOTE, or a default from `exocomp login`")
	}

	view, err := rc.Reload(cmd.Context())
	if err != nil {
		return remoteFail(cmd, mode, err)
	}

	if mode == outputJSON {
		return emitJSON(cmd, wire.KindMaintenanceReload, view)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "schedules reloaded (%d enabled in the store)\n", view.Schedules)
	return err
}
