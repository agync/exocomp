package main

import (
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newJobsCmd)
}

func newJobsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "jobs",
		Short:   "Inspect the job queue and a job's events",
		Example: "exocomp jobs list --state running --output json",
		Long: "Read-only views over the one job queue: what is queued, running, or " +
			"settled, and the append-only, already-redacted event log of a single run. " +
			"An agent branches on state and exit_code; the JSON is values-free. `follow` " +
			"is the one verb here that waits, and it exits with the job's own code.",
	}
	cmd.AddCommand(newJobsListCmd(), newJobsGetCmd(), newJobsEventsCmd(), newJobsFollowCmd())
	return cmd
}

// newJobsFollowCmd watches one job to its end. It is the only jobs verb that
// waits, and so the only one whose exit code is the JOB's: a follow is "wait for
// this job", and what a waiter reports is the thing it waited for. Remotely it
// consumes the daemon's SSE stream; locally it tails the same append-only
// job_event rows the daemon writes.
func newJobsFollowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "follow <id>",
		Short: "Watch a job's events until it settles",
		Long: "Prints the job's already-redacted events as they land and exits with the " +
			"job's own code once it is terminal: 0 for a success (a degraded run included), " +
			"otherwise the run's exit code. A job that is already terminal prints its log " +
			"and returns at once. With --remote the events arrive over the daemon's event " +
			"stream, which resumes from the last sequence if the connection drops.",
		Example: "exocomp --remote box jobs follow 42",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("jobs follow: %q is not a job id", args[0])
			}

			rc, remote, err := remoteClient(cmd)
			if err != nil {
				return err
			}
			if remote {
				code, err := followRemoteJob(cmd, rc, mode, id)
				if err != nil {
					return remoteFail(cmd, mode, err)
				}
				return exitWith(code, nil)
			}
			return exitWith(followLocalJob(cmd, mode, id))
		},
	}
}

// followLocalJob tails a local job through the same reader the CLI's own
// daemon-enqueue path uses, so a followed job prints what an enqueuing command
// would have printed.
func followLocalJob(cmd *cobra.Command, mode string, id int64) (int, error) {
	dir, err := dataDir()
	if err != nil {
		return 0, err
	}
	st, err := openStore()
	if err != nil {
		return 0, err
	}
	defer func() { _ = st.Close() }()

	out := cmd.OutOrStdout()
	sink := out
	if mode == outputJSON {
		// JSON mode emits ONE document: the settled job, not a stream of lines a
		// caller did not ask to parse.
		sink = io.Discard
	}
	final, err := tailDaemonJob(cmd.Context(), st, dir, id, sink)
	if err != nil {
		if mode == outputJSON {
			return 0, failJSON(cmd, err)
		}
		return 0, err
	}
	if mode == outputJSON {
		return reportJobOutcome(io.Discard, final), emitJSON(cmd, wire.KindJob, wire.NewJobView(final))
	}
	return reportJobOutcome(out, final), nil
}

func newJobsListCmd() *cobra.Command {
	var (
		states         []string
		runbook        string
		concurrencyKey string
		limit          int
	)
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List jobs, oldest first",
		Example: "exocomp jobs list --state running --state queued",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			filter := store.JobFilter{Runbook: runbook, ConcurrencyKey: concurrencyKey, Limit: limit}
			for _, s := range states {
				if !knownJobState(s) {
					return fmt.Errorf("jobs list: --state %q is not a job state", s)
				}
				filter.States = append(filter.States, store.JobState(s))
			}

			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				rows, err := rc.ListJobs(cmd.Context(), client.JobFilter{
					States: states, Runbook: runbook, ConcurrencyKey: concurrencyKey, Limit: limit,
				})
				if err != nil {
					return remoteFail(cmd, mode, err)
				}
				return renderRemoteJobsList(cmd, mode, rows)
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListJobs(cmd.Context(), filter)
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindJobList, wire.NewJobList(rows))
			}

			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no jobs")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tSTATE\tRUNBOOK\tKIND\tTRIGGER\tEXIT\tQUEUED")
			for _, j := range rows {
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
					j.ID, j.State, dashIfEmpty(j.Runbook), dashIfEmpty(j.Kind),
					dashIfEmpty(j.Trigger), exitCodeLabel(j.ExitCode),
					j.QueuedAt.UTC().Format("2006-01-02 15:04:05Z"))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringSliceVar(&states, "state", nil, "only jobs in this state (repeatable)")
	cmd.Flags().StringVar(&runbook, "runbook", "", "only jobs for this runbook")
	cmd.Flags().StringVar(&concurrencyKey, "concurrency-key", "", "only jobs under this concurrency key")
	cmd.Flags().IntVar(&limit, "limit", 0, "how many jobs to show (0: all)")
	return cmd
}

func newJobsGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "get <id>",
		Short:   "Show one job",
		Example: "exocomp jobs get 42 --output json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("jobs get: %q is not a job id", args[0])
			}

			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				job, err := rc.GetJob(cmd.Context(), id)
				if err != nil {
					return remoteFail(cmd, mode, err)
				}
				return renderRemoteJobGet(cmd, mode, job)
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			job, err := st.GetJob(cmd.Context(), id)
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindJob, wire.NewJobView(job))
			}

			out := cmd.OutOrStdout()
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintf(w, "id\t%d\n", job.ID)
			_, _ = fmt.Fprintf(w, "state\t%s\n", job.State)
			_, _ = fmt.Fprintf(w, "runbook\t%s\n", dashIfEmpty(job.Runbook))
			_, _ = fmt.Fprintf(w, "kind\t%s\n", dashIfEmpty(job.Kind))
			_, _ = fmt.Fprintf(w, "trigger\t%s\n", dashIfEmpty(job.Trigger))
			_, _ = fmt.Fprintf(w, "exit\t%s\n", exitCodeLabel(job.ExitCode))
			_, _ = fmt.Fprintf(w, "queued\t%s\n", job.QueuedAt.UTC().Format("2006-01-02 15:04:05Z"))
			return w.Flush()
		},
	}
	return cmd
}

func newJobsEventsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "events <id>",
		Short:   "Show a job's append-only event log",
		Example: "exocomp jobs events 42 --output json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("jobs events: %q is not a job id", args[0])
			}

			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				rows, err := rc.JobEvents(cmd.Context(), id, 0)
				if err != nil {
					return remoteFail(cmd, mode, err)
				}
				return renderRemoteJobEvents(cmd, mode, rows)
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListJobEvents(cmd.Context(), id)
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindJobEvents, wire.NewJobEventList(rows))
			}

			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no events")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "SEQ\tTS\tLEVEL\tMSG")
			for _, e := range rows {
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n",
					e.Seq, e.Ts.UTC().Format("2006-01-02 15:04:05Z"), e.Level, e.Msg)
			}
			return w.Flush()
		},
	}
	return cmd
}

func knownJobState(s string) bool {
	switch store.JobState(s) {
	case store.StateQueued, store.StateAwaitingApproval, store.StateRunning,
		store.StateSucceeded, store.StateFailed, store.StateCancelled,
		store.StateTimedOut, store.StateInterrupted:
		return true
	default:
		return false
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func exitCodeLabel(code *int) string {
	if code == nil {
		return "-"
	}
	return strconv.Itoa(*code)
}
