package main

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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
	cmd.AddCommand(
		newScheduleAddCmd(),
		newScheduleListCmd(),
		newScheduleRmCmd(),
		newScheduleEnableCmd(),
		newScheduleDisableCmd(),
	)
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

func newScheduleEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <name>",
		Short: "Let a schedule fire again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setScheduleEnabled(cmd, args[0], true)
		},
	}
}

func newScheduleDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <name>",
		Short: "Stop a schedule from firing, keeping its definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setScheduleEnabled(cmd, args[0], false)
		},
	}
}

// setScheduleEnabled flips the stored flag. A running daemon holds its
// registrations in memory, so the change lands there on the next reload —
// which is why the confirmation says how to make that happen now.
func setScheduleEnabled(cmd *cobra.Command, name string, enabled bool) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.SetScheduleEnabled(cmd.Context(), name, enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("schedule %s: %q not found", enabledVerb(enabled), name)
		}
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"schedule %s %s; a running `exocomp serve` picks it up on its next reload, or at once on SIGHUP\n",
		name, enabledLabel(enabled))
	return err
}

func enabledVerb(enabled bool) string {
	if enabled {
		return "enable"
	}
	return "disable"
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
		Short: "Deprecated: the schedule daemon is `exocomp serve`",
	}
	cmd.AddCommand(newSchedulerRunCmd())
	return cmd
}

// newSchedulerRunCmd keeps the M2a entry point working as an alias. Its own
// process — scheduler plus queue, without the state backend — is gone: one
// daemon owns every surface, and two of them would fight over the same rows.
func newSchedulerRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:        "run",
		Short:      "Deprecated alias for `exocomp serve`",
		Deprecated: "run `exocomp serve`: one process serves the state backend, drains the queue, and fires schedules.",
		Args:       cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), defaultServeOptions())
		},
	}
}
