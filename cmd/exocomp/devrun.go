package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newDevRunCmd)
}

// newDevRunCmd is M1 scaffolding: it exercises the store, the queue and the
// shell driver end to end so M1b and M1c have something to verify against
// before the service and its surfaces exist.
func newDevRunCmd() *cobra.Command {
	var (
		timeout        time.Duration
		concurrencyKey string
	)

	cmd := &cobra.Command{
		Use:    "devrun -- <argv...>",
		Short:  "Run one shell job through the queue",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDevRun(cmd.Context(), cmd.OutOrStdout(), devRunSpec{
				argv:           args,
				timeout:        timeout,
				concurrencyKey: concurrencyKey,
				idempotencyKey: idempotencyKey(cmd),
			})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0,
		"kill the job after this long (0: no limit)")
	cmd.Flags().StringVar(&concurrencyKey, "concurrency-key", "",
		"serialize this job against others sharing the key")
	return cmd
}

type devRunSpec struct {
	argv           []string
	timeout        time.Duration
	concurrencyKey string
	// idempotencyKey dedups a retried job to one row; empty on the unchanged path.
	idempotencyKey string
}

func runDevRun(ctx context.Context, w io.Writer, spec devRunSpec) (err error) {
	if ctx == nil {
		ctx = context.Background()
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

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      1,
		PollInterval: 50 * time.Millisecond,
	})
	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook:        "devrun",
		Kind:           "shell",
		Trigger:        "manual",
		Argv:           spec.argv,
		ConcurrencyKey: spec.concurrencyKey,
		IdempotencyKey: spec.idempotencyKey,
		Timeout:        spec.timeout,
	})
	if err != nil {
		return err
	}
	if err := q.Drain(ctx); err != nil {
		return err
	}

	final, err := st.GetJob(ctx, job.ID)
	if err != nil {
		return err
	}
	events, err := st.LastJobEvents(ctx, job.ID, 5)
	if err != nil {
		return err
	}
	printJob(w, final, events)

	if final.State != store.StateSucceeded {
		return fmt.Errorf("devrun: job %d ended %s", final.ID, final.State)
	}
	return nil
}

func printJob(w io.Writer, job store.Job, events []store.JobEvent) {
	exit := "-"
	if job.ExitCode != nil {
		exit = fmt.Sprintf("%d", *job.ExitCode)
	}
	_, _ = fmt.Fprintf(w, "job %d  state: %s  exit: %s  duration: %s\n",
		job.ID, job.State, exit, jobDuration(job))

	_, _ = fmt.Fprintf(w, "last %d event(s):\n", len(events))
	for _, e := range events {
		_, _ = fmt.Fprintf(w, "  %d [%s] %s\n", e.Seq, e.Level, e.Msg)
	}
}

func jobDuration(job store.Job) string {
	if job.StartedAt == nil || job.FinishedAt == nil {
		return "-"
	}
	return job.FinishedAt.Sub(*job.StartedAt).Round(time.Millisecond).String()
}
