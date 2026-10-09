package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newRestartCmd)
}

// restartWaitPoll is how often `restart --wait` re-asks the daemon whether its
// drain is still pending.
const restartWaitPoll = time.Second

// defaultRestartWaitTimeout bounds `restart --wait`, so a job that never ends
// does not wedge the command forever. It matches the daemon's own drain bound.
const defaultRestartWaitTimeout = 10 * time.Minute

// jobContextEnv is the marker the executor sets on every job subprocess
// (internal/executor's Shell.Run, from queue.Spec.JobID). A non-empty value
// means this process is itself a queued job, not an operator's shell.
const jobContextEnv = "EXOCOMP_JOB_ID"

// inJobContext reports whether this process runs as a queued job.
func inJobContext() bool { return os.Getenv(jobContextEnv) != "" }

func newRestartCmd() *cobra.Command {
	var (
		wait    bool
		force   bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon after draining its in-flight jobs",
		Long: "Cordons a running daemon, lets the jobs already running finish, then " +
			"shuts it down cleanly and re-execs the on-disk binary in place — same pid, no " +
			"systemd restart. It is for a binary or serve-flag change; a schedule change is " +
			"`exocomp reload`.\n\n" +
			"The request is a one-way signal: it returns at once and the drain runs in the " +
			"daemon's background. --wait then polls until the daemon goes down (the restart " +
			"fired) and reports it. A drain that cannot finish within the daemon's bound is " +
			"aborted and the cordon lifted; --force cancels the remaining jobs and restarts " +
			"instead — those jobs land cancelled and are re-drivable with `exocomp jobs redrive`.\n\n" +
			"Inside a job, --wait is refused: the requesting job keeps the daemon draining " +
			"until the job exits, so waiting on the restart would deadlock. A job must use " +
			"the bare (fire-and-forget) form.\n\n" +
			"restart controls a RUNNING daemon over the operator API, so it needs a selected " +
			"remote (--remote or EXOCOMP_REMOTE) — the control node targets its own daemon.",
		Example: "exocomp --remote self restart --wait",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRestart(cmd, restartOptions{wait: wait, force: force, timeout: timeout})
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the daemon goes down, then report it is restarting (refused inside a job)")
	cmd.Flags().BoolVar(&force, "force", false, "cancel the remaining jobs instead of aborting when the drain does not finish")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultRestartWaitTimeout, "bound for --wait")
	return cmd
}

type restartOptions struct {
	wait    bool
	force   bool
	timeout time.Duration
}

// runRestart asks the selected remote daemon to restart after draining. The
// request is a one-way signal — the daemon answers the pending state and drains
// in the background — so a bare restart reports that state and returns; --wait
// polls the status until the daemon goes down.
func runRestart(cmd *cobra.Command, opt restartOptions) error {
	mode, err := outputMode(cmd)
	if err != nil {
		return err
	}

	// A job that asks for a restart must not wait on it: the daemon's drain
	// counts the requesting job as in flight until it is terminal, so `--wait`
	// would poll for the whole bound and then abort — a self-deadlock. The bare
	// form returns at once and is the correct call from inside a job.
	if opt.wait && inJobContext() {
		return errors.New("restart --wait cannot run inside a job: the requesting job " +
			"keeps the daemon draining until it exits, so waiting would deadlock; " +
			"run the bare `restart` (fire-and-forget) instead")
	}

	rc, remote, err := remoteClient(cmd)
	if err != nil {
		return err
	}
	if !remote {
		return errors.New("restart controls a running daemon; select it with --remote (or EXOCOMP_REMOTE)")
	}

	view, err := rc.Restart(cmd.Context(), opt.force)
	if err != nil {
		return remoteFail(cmd, mode, err)
	}
	if !opt.wait {
		if err := renderRestart(cmd, mode, view); err != nil {
			return err
		}
		return maybeRedriveHint(cmd, mode, opt.force, view.InFlight)
	}

	out := cmd.OutOrStdout()
	if mode != outputJSON {
		_, _ = fmt.Fprintf(out, "restart requested: draining %d job(s)\n", view.InFlight)
	}
	if err := maybeRedriveHint(cmd, mode, opt.force, view.InFlight); err != nil {
		return err
	}
	return waitForRestart(cmd, rc, mode, view, opt.timeout)
}

// maybeRedriveHint points a forced restart at the recovery verb: --force cancels
// the jobs still in flight, which land cancelled/interrupted, so the operator
// knows what to re-drive rather than discovering it later. Text only — JSON
// stdout must stay parseable.
func maybeRedriveHint(cmd *cobra.Command, mode string, force bool, inFlight int) error {
	if mode == outputJSON || !force || inFlight == 0 {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"forced: %d in-flight job(s) were cancelled; re-drive survivors with `exocomp jobs redrive`\n", inFlight)
	return err
}

// waitForRestart polls the daemon's restart status until it goes down (the
// restart fired) or the pending flag clears (the drain aborted). It never holds
// the daemon open: a dropped connection is the success signal, not an error.
func waitForRestart(cmd *cobra.Command, rc *client.Client, mode string, view wire.RestartView, timeout time.Duration) error {
	out := cmd.OutOrStdout()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(restartWaitPoll)
	defer ticker.Stop()

	for {
		if !time.Now().Before(deadline) {
			if mode == outputJSON {
				cmd.SilenceErrors = true
				_ = emitJSON(cmd, wire.KindMaintenanceRestart, view)
			}
			return errors.New("restart: still draining within the timeout")
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-ticker.C:
		}

		next, err := rc.RestartStatus(cmd.Context())
		if err != nil {
			if daemonDown(err) {
				// The daemon closed the listener for the re-exec: the restart is
				// happening. A script reads the drop as success.
				if mode == outputJSON {
					return emitJSON(cmd, wire.KindMaintenanceRestart, view)
				}
				_, _ = fmt.Fprintln(out, "daemon restarting")
				return nil
			}
			return remoteFail(cmd, mode, err)
		}
		view = next
		if !next.Pending {
			if mode == outputJSON {
				cmd.SilenceErrors = true
				_ = emitJSON(cmd, wire.KindMaintenanceRestart, next)
			}
			return errors.New("restart aborted: the drain did not finish and the cordon was lifted")
		}
		if mode != outputJSON {
			_, _ = fmt.Fprintf(out, "waiting: %d job(s) in flight\n", next.InFlight)
		}
	}
}

// daemonDown reports whether a status poll failed because the daemon is going
// down for its re-exec rather than because it answered with an error. A decoded
// wire error (an APIError) is the daemon still up and refusing; anything else is
// the connection dropping.
func daemonDown(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *client.APIError
	return !errors.As(err, &apiErr)
}

// renderRestart prints a restart's pending state. JSON re-emits the daemon's
// restart envelope; text is a one-line human summary.
func renderRestart(cmd *cobra.Command, mode string, view wire.RestartView) error {
	if mode == outputJSON {
		return emitJSON(cmd, wire.KindMaintenanceRestart, view)
	}
	out := cmd.OutOrStdout()
	if !view.Pending {
		_, err := fmt.Fprintln(out, "no restart pending")
		return err
	}
	deadline := view.Deadline
	if deadline == "" {
		deadline = "-"
	}
	_, err := fmt.Fprintf(out, "restart requested: draining %d job(s)  deadline %s\n",
		view.InFlight, deadline)
	return err
}
