package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newScheduleCmd)
	registerCommand(newSchedulerCmd)
}

func newScheduleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Manage recurring jobs",
	}
	cmd.AddCommand(newScheduleAddCmd(), newScheduleListCmd(), newScheduleRmCmd())
	return cmd
}

func newScheduleAddCmd() *cobra.Command {
	var (
		cronExpr       string
		kind           string
		target         string
		concurrencyKey string
		jitter         int
	)

	cmd := &cobra.Command{
		Use:   "add <name> [-- <argv...>]",
		Short: "Register a recurring job",
		Long: "Kind shell runs argv (everything after --) on the cron schedule. Kind " +
			"backup runs one target's leg (--target), or every enabled target when " +
			"--target is empty, each as its own job.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, argv := args[0], args[1:]

			var scheduleArgs store.ScheduleArgs
			switch kind {
			case store.ScheduleShell:
				if len(argv) == 0 {
					return errors.New("schedule add: kind shell needs argv after --")
				}
				scheduleArgs.Argv = argv
			case store.ScheduleBackup:
				if len(argv) != 0 {
					return errors.New("schedule add: kind backup takes no argv")
				}
				scheduleArgs.Target = target
			default:
				return fmt.Errorf("schedule add: --kind must be %s or %s", store.ScheduleShell, store.ScheduleBackup)
			}
			if jitter < 0 {
				return errors.New("schedule add: --jitter cannot be negative")
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			created, err := st.CreateSchedule(cmd.Context(), store.NewSchedule{
				Name:           name,
				CronExpr:       cronExpr,
				Kind:           kind,
				Args:           scheduleArgs,
				ConcurrencyKey: concurrencyKey,
				JitterS:        jitter,
			})
			if err != nil {
				if errors.Is(err, store.ErrDuplicate) {
					return fmt.Errorf("schedule add: %q already exists", name)
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "schedule %s (%s) added: %s\n",
				created.Name, created.Kind, created.CronExpr)
			return err
		},
	}
	cmd.Flags().StringVar(&cronExpr, "cron", "",
		"cron expression: 6 fields (seconds first), or \"@every 5s\"")
	cmd.Flags().StringVar(&kind, "kind", "", "shell|backup")
	cmd.Flags().StringVar(&target, "target", "", "backup kind: target name (empty: every enabled target)")
	cmd.Flags().StringVar(&concurrencyKey, "concurrency-key", "", "shell kind: serialize against jobs sharing this key")
	cmd.Flags().IntVar(&jitter, "jitter", 0, "sleep a random number of seconds up to this before enqueueing")
	_ = cmd.MarkFlagRequired("cron")
	_ = cmd.MarkFlagRequired("kind")
	return cmd
}

func newScheduleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List schedules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListSchedules(cmd.Context(), false)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tCRON\tKIND\tSTATE\tLAST JOB\tNEXT AT")
			for _, s := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					s.Name, s.CronExpr, s.Kind, enabledLabel(s.Enabled),
					lastJobIDLabel(s.LastJobID), nextAtLabel(s.NextAt))
			}
			return w.Flush()
		},
	}
}

func newScheduleRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a schedule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.DeleteSchedule(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("schedule rm: %q not found", args[0])
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "schedule %s removed\n", args[0])
			return err
		},
	}
}

func lastJobIDLabel(id *int64) string {
	if id == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *id)
}

func nextAtLabel(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func newSchedulerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scheduler",
		Short: "Run the schedule daemon",
	}
	cmd.AddCommand(newSchedulerRunCmd())
	return cmd
}

func newSchedulerRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Fire schedules and drain the queue until interrupted",
		Long: "The M2 stand-in for exocompd: it fires schedules through the same queue " +
			"as every other execution path and runs until SIGINT or SIGTERM. It goes away " +
			"once `exocomp serve` unifies every surface into one long-running process.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runScheduler(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func runScheduler(ctx context.Context, out, warn io.Writer) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("scheduler run: resolve data dir: %w", err)
	}

	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("scheduler run: %w", err)
	}
	defer func() { _ = env.Close() }()

	st, err := openJobStore(ctx, warn)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	runner := backup.NewRunner(st, env, backup.Options{}, targetEnv)
	drv := sched.NewDriver(executor.NewShell(), runner)
	q := queue.New(st, drv, queue.Options{Workers: 4, PollInterval: 200 * time.Millisecond})

	s := sched.New(st, q, nil)
	if err := s.Start(ctx); err != nil {
		return fmt.Errorf("scheduler run: %w", err)
	}

	qDone := make(chan error, 1)
	go func() { qDone <- q.Run(ctx) }()

	_, _ = fmt.Fprintln(out, "scheduler running (ctrl-c to stop)")
	<-ctx.Done()

	s.Stop()
	return <-qDone
}
