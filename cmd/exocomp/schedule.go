package main

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
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
		check          bool
		tolerateUnre   bool
		sourceName     string
	)

	cmd := &cobra.Command{
		Use:   "add <name> [-- <argv...>]",
		Short: "Register a recurring job",
		Long: "Kind shell runs argv (everything after --) on the cron schedule. Kind " +
			"backup runs one target's leg (--target), or every enabled target when " +
			"--target is empty, each as its own job. Kind play runs a playbook " +
			"(everything after --, the playbook first) against the standing inventory " +
			"--target names — `tf:<workspace>` for the inventory a workspace's apply " +
			"refreshes, so a newly provisioned host joins the next fire. A play whose " +
			"inventory is missing, stale, or hostless enqueues nothing. With --source the " +
			"playbook is relative to that registered source's checkout, the play runs from " +
			"it (so its ansible.cfg loads), and every fire inherits the source's default " +
			"egress ports (`exocomp source egress`) and run-secrets binding " +
			"(`exocomp source run-secrets`). The inventory is still the standing one --target " +
			"names, so group_vars beside the checkout's own inventory are not read, and a " +
			"schedule has no vault project. Without --source a play fire names no " +
			"source, uses the playbook path as given, and inherits neither. A source with " +
			"lifecycle hooks is refused, as for a playbook rule.\n\n" +
			"Kind play with --check is the config-drift schedule: it fires the play in " +
			"ansible check mode (--check --diff) on the cron, so every fire emits the " +
			"structured 'would change' outcome as a job event without touching a host " +
			"— the config twin of a tofu-plan drift schedule (docs/operations/playbook-run.md).\n\n" +
			"Kind play with --tolerate-unreachable is the roaming-fleet converge: a fire " +
			"whose only non-success is unreachable hosts settles degraded (not failed) and " +
			"does not page, so a scheduled site.yml against a fleet with a laptop that sleeps " +
			"keeps converging the online hosts instead of paging every night " +
			"(docs/operations/adhoc-and-partial.md).\n\n" +
			"Kind drill runs a restore drill against one target (--target), or every " +
			"enabled target when --target is empty, each as its own job — the monthly " +
			"dr-drill (docs/operations/disaster-recovery.md) as a cadence instead of a " +
			"manual `exocomp backup drill`. A drill that finds a bad backup settles the " +
			"job failed, which the job-failure sweep pages on like any other.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, argv := args[0], args[1:]

			if check && kind != sched.KindPlay {
				return errors.New("schedule add: --check applies to kind play (the config-drift schedule)")
			}
			if tolerateUnre && kind != sched.KindPlay {
				return errors.New("schedule add: --tolerate-unreachable applies to kind play")
			}
			if sourceName != "" && kind != sched.KindPlay {
				return errors.New("schedule add: --source applies to kind play")
			}

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
				// A backup fire builds its own key per target
				// ("backup:<target>"), so anything given here would be
				// dropped on the floor.
				if concurrencyKey != "" {
					return errors.New("schedule add: kind backup keys each fire by target; " +
						"--concurrency-key applies to kind shell only")
				}
				scheduleArgs.Target = target
			case sched.KindPlay:
				if len(argv) == 0 {
					return errors.New("schedule add: kind play needs a playbook after --")
				}
				if target == "" {
					return errors.New("schedule add: kind play needs --target <inventory>")
				}
				scheduleArgs.Argv = argv
				scheduleArgs.Target = target
				scheduleArgs.Source = sourceName
				scheduleArgs.TolerateUnreachable = tolerateUnre
				if check {
					// The config-drift schedule: --check --diff go after the
					// playbook, where AnsibleArtifactArgv reads them as the
					// passthrough options that put the fire in check mode. The
					// ansible driver then emits the structured drift outcome.
					scheduleArgs.Argv = append(scheduleArgs.Argv, "--check", "--diff")
				}
			case store.ScheduleDrill:
				if len(argv) != 0 {
					return errors.New("schedule add: kind drill takes no argv")
				}
				// A drill fire builds its own key per target ("drill:<target>"),
				// the same reason kind backup refuses one here.
				if concurrencyKey != "" {
					return errors.New("schedule add: kind drill keys each fire by target; " +
						"--concurrency-key applies to kind shell only")
				}
				scheduleArgs.Target = target
			default:
				return fmt.Errorf("schedule add: --kind must be %s, %s, %s or %s",
					store.ScheduleShell, store.ScheduleBackup, sched.KindPlay, store.ScheduleDrill)
			}
			ns := store.NewSchedule{
				Name:           name,
				CronExpr:       cronExpr,
				Kind:           kind,
				Args:           scheduleArgs,
				ConcurrencyKey: concurrencyKey,
				JitterS:        jitter,
			}
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				return runRemoteScheduleAdd(cmd, rc, ns)
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			dir, err := dataDir()
			if err != nil {
				return fmt.Errorf("schedule add: resolve data dir: %w", err)
			}
			// The checks a fire would hit (the cron, the kind's fields, a source
			// play that resolves) run here, so a schedule that could never fire is
			// refused now rather than logged at every fire.
			if err := sched.CheckNewSchedule(ns); err != nil {
				return fmt.Errorf("schedule add: %w", validationError(err))
			}
			created, err := service.New(st, dir, nil).CreateSchedule(cmd.Context(), ns, service.Actor{})
			if err != nil {
				if errors.Is(err, store.ErrDuplicate) {
					return fmt.Errorf("schedule add: %q already exists", name)
				}
				return fmt.Errorf("schedule add: %w", validationError(err))
			}
			printScheduleAdded(cmd, created.Name, created.Kind, created.CronExpr)
			return nil
		},
	}
	cmd.Flags().StringVar(&cronExpr, "cron", "",
		"cron expression: 6 fields (seconds first), or \"@every 5s\"")
	cmd.Flags().StringVar(&kind, "kind", "", "shell|backup|play|drill")
	cmd.Flags().StringVar(&target, "target", "", "backup/drill kind: target name (empty: every enabled target); play kind: standing inventory name")
	cmd.Flags().StringVar(&concurrencyKey, "concurrency-key", "", "shell/play kinds: serialize against jobs sharing this key")
	cmd.Flags().IntVar(&jitter, "jitter", 0, "sleep a random number of seconds up to this before enqueueing")
	cmd.Flags().StringVar(&sourceName, "source", "", "play kind: run from this registered source's checkout; the playbook is relative to it and the fire inherits the source's default egress and run-secrets")
	cmd.Flags().BoolVar(&check, "check", false, "play kind: run in ansible check mode (--check --diff) as the config-drift schedule")
	cmd.Flags().BoolVar(&tolerateUnre, "tolerate-unreachable", false, "play kind: a fire whose only non-success is unreachable hosts settles degraded, not failed (a roaming host asleep does not page)")
	_ = cmd.MarkFlagRequired("cron")
	_ = cmd.MarkFlagRequired("kind")
	return cmd
}

// printScheduleAdded confirms a new schedule.
func printScheduleAdded(cmd *cobra.Command, name, kind, cron string) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "schedule %s (%s) added: %s\n", name, kind, cron)
}

func newScheduleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List schedules",
		Example: "exocomp schedule list --output json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			var views []wire.ScheduleView
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				if views, err = rc.ListSchedules(cmd.Context()); err != nil {
					return remoteFail(cmd, mode, err)
				}
			} else {
				st, err := openStore()
				if err != nil {
					return err
				}
				defer func() { _ = st.Close() }()
				rows, err := st.ListSchedules(cmd.Context(), false)
				if err != nil {
					return err
				}
				views = wire.NewScheduleList(rows)
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindScheduleList, views)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tCRON\tKIND\tSTATE\tLAST JOB\tNEXT AT")
			for _, s := range views {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					s.Name, s.Cron, s.Kind, enabledLabel(s.Enabled),
					lastJobIDLabel(s.LastJobID), nextAtLabel(parseWireTime(s.NextAt)))
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
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				return runRemoteScheduleRm(cmd, rc, args[0])
			}
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
	if rc, ok, err := remoteClient(cmd); err != nil {
		return err
	} else if ok {
		return runRemoteScheduleSetEnabled(cmd, rc, name, enabled)
	}
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

// newSchedulerRunCmd keeps the legacy entry point working as an alias. Its own
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
