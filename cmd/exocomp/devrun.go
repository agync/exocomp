package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
}

func runDevRun(ctx context.Context, w io.Writer, spec devRunSpec) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("devrun: resolve data dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("devrun: create data dir: %w", err)
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	swept, err := st.SweepInterrupted(ctx)
	if err != nil {
		return err
	}
	if swept > 0 {
		_, _ = fmt.Fprintf(w, "swept %d job(s) left by a previous process to interrupted\n", swept)
	}

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
