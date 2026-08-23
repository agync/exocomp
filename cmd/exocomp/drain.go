package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newDrainCmd)
}

// drainPollInterval is how often `drain --wait` re-asks the daemon whether it is
// safe to reboot yet.
const drainPollInterval = time.Second

// defaultDrainWaitTimeout bounds `drain --wait`, so a job that never ends does
// not wedge a reboot script forever.
const defaultDrainWaitTimeout = 10 * time.Minute

func newDrainCmd() *cobra.Command {
	var (
		resume  bool
		status  bool
		wait    bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Cordon the daemon for a planned reboot and report when it is safe",
		Long: "Puts a running daemon into maintenance mode ahead of a planned reboot: no " +
			"new schedule fires, no new job claims, while jobs already running finish (they " +
			"are NOT cancelled). It then reports how many jobs are still in flight and whether " +
			"a reboot is safe (cordoned AND nothing in flight).\n\n" +
			"--wait blocks until safe or --timeout; --resume lifts the cordon; --status only " +
			"reports readiness without cordoning. `drain interrupted` lists the jobs a reboot " +
			"left interrupted, for re-driving afterwards.\n\n" +
			"drain controls a RUNNING daemon over the operator API, so it needs a selected " +
			"remote (--remote or EXOCOMP_REMOTE) — the control node targets its own daemon.",
		Example: "exocomp --remote box drain --wait",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDrain(cmd, drainOptions{resume: resume, status: status, wait: wait, timeout: timeout})
		},
	}
	cmd.Flags().BoolVar(&resume, "resume", false, "lift the cordon (undrain) instead of applying it")
	cmd.Flags().BoolVar(&status, "status", false, "report readiness only; cordon nothing")
	cmd.Flags().BoolVar(&wait, "wait", false, "after cordoning, block until it is safe to reboot")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultDrainWaitTimeout, "bound for --wait")
	cmd.AddCommand(newDrainInterruptedCmd())
	return cmd
}

type drainOptions struct {
	resume  bool
	status  bool
	wait    bool
	timeout time.Duration
}

// runDrain applies (or lifts, or just reports) the cordon on the selected remote
// daemon. cordon control is live daemon state, so it always goes over the API —
// there is no local in-process daemon a separate CLI could poke, and a signal
// could not return the in-flight count the readiness answer needs.
func runDrain(cmd *cobra.Command, opt drainOptions) error {
	mode, err := outputMode(cmd)
	if err != nil {
		return err
	}
	if opt.resume && (opt.status || opt.wait) {
		return errors.New("drain: --resume takes no --wait or --status")
	}

	rc, remote, err := remoteClient(cmd)
	if err != nil {
		return err
	}
	if !remote {
		return errors.New("drain controls a running daemon; select it with --remote (or EXOCOMP_REMOTE)")
	}

	ctx := cmd.Context()
	var view wire.MaintenanceView
	switch {
	case opt.resume:
		view, err = rc.Uncordon(ctx)
	case opt.status:
		view, err = rc.Readiness(ctx)
	default:
		view, err = rc.Cordon(ctx)
	}
	if err != nil {
		return remoteFail(cmd, mode, err)
	}

	if opt.wait && !view.SafeToReboot {
		view, err = waitUntilSafe(cmd, rc, mode, opt.timeout)
		if err != nil {
			return err
		}
	}

	if err := renderReadiness(cmd, mode, opt, view); err != nil {
		return err
	}
	if opt.wait && !view.SafeToReboot {
		// A --wait that timed out short of safe must exit nonzero, so a reboot
		// script does not proceed on a daemon still finishing work. The readiness
		// envelope above is the machine answer; on the JSON path the error only
		// carries the exit code, so it is not printed as a second document.
		if mode == outputJSON {
			cmd.SilenceErrors = true
		}
		return errors.New("drain: still not safe to reboot within the timeout")
	}
	return nil
}

// waitUntilSafe polls readiness until the daemon is safe to reboot or the timeout
// passes, printing progress on the text path.
func waitUntilSafe(cmd *cobra.Command, rc *client.Client, mode string, timeout time.Duration) (wire.MaintenanceView, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The deadline (or an interrupt) ended the wait; report the last known
			// state to the caller, which decides the exit code.
			view, err := rc.Readiness(cmd.Context())
			if err != nil {
				return wire.MaintenanceView{}, remoteFail(cmd, mode, err)
			}
			return view, nil
		case <-ticker.C:
			view, err := rc.Readiness(cmd.Context())
			if err != nil {
				return wire.MaintenanceView{}, remoteFail(cmd, mode, err)
			}
			if mode != outputJSON {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "waiting: %d job(s) in flight\n", view.InFlight)
			}
			if view.SafeToReboot {
				return view, nil
			}
		}
	}
}

// renderReadiness prints the readiness answer. JSON re-emits the daemon's own
// maintenance envelope; text is a one-line human summary.
func renderReadiness(cmd *cobra.Command, mode string, opt drainOptions, view wire.MaintenanceView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindMaintenance, view)
	}
	out := cmd.OutOrStdout()
	if opt.resume {
		_, err := fmt.Fprintln(out, "cordon lifted; the daemon is taking work again")
		return err
	}
	safe := "not yet"
	if view.SafeToReboot {
		safe = "yes"
	}
	_, err := fmt.Fprintf(out, "cordoned: %s  in-flight: %d  safe to reboot: %s\n",
		yesNo(view.Cordoned), view.InFlight, safe)
	return err
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// newDrainInterruptedCmd lists the jobs a reboot (or crash) left interrupted.
// Interrupted is terminal and never auto-retried; this is the operator's view of
// what to re-drive. It works locally (reads the store) and remotely (the daemon's
// interrupted listing), like `jobs list`.
func newDrainInterruptedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "interrupted",
		Short: "List jobs a reboot left interrupted, newest first",
		Long: "The runs a shutdown or crash cut off, retired to the terminal interrupted " +
			"state and never auto-retried. Re-drive the ones that still matter.",
		Example: "exocomp --remote box drain interrupted --output json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			rc, remote, err := remoteClient(cmd)
			if err != nil {
				return err
			}
			if remote {
				rows, err := rc.ListInterrupted(cmd.Context())
				if err != nil {
					return remoteFail(cmd, mode, err)
				}
				return renderInterrupted(cmd, mode, rows)
			}
			return runLocalInterrupted(cmd, mode)
		},
	}
}

// runLocalInterrupted reads the interrupted jobs straight from this box's store,
// the post-reboot case where the operator is on the control node.
func runLocalInterrupted(cmd *cobra.Command, mode string) error {
	st, err := openStore()
	if err != nil {
		if mode == outputJSON {
			return failJSON(cmd, err)
		}
		return err
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListInterruptedJobs(cmd.Context(), 0)
	if err != nil {
		if mode == outputJSON {
			return failJSON(cmd, err)
		}
		return err
	}
	return renderInterrupted(cmd, mode, wire.NewJobList(rows))
}

// renderInterrupted prints the interrupted listing, reusing the jobs-list
// columns so a caller reads one job shape everywhere.
func renderInterrupted(cmd *cobra.Command, mode string, rows []wire.JobView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindJobList, rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no interrupted jobs")
		return err
	}
	return renderRemoteJobsList(cmd, mode, rows)
}
