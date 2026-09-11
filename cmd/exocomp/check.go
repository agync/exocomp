package main

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newCheckCmd)
}

func newCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Manage probe checks",
		Long: "A check is a named probe set. Each --target names one probe; the " +
			"daemon's sweep answers each one on its interval and emits one event " +
			"into the reactive pipeline when a target's answer flips " +
			"(docs/design/check-triggered-runs.md).",
	}
	cmd.AddCommand(
		newCheckAddCmd(),
		newCheckUpdateCmd(),
		newCheckListCmd(),
		newCheckEnableCmd(),
		newCheckDisableCmd(),
		newCheckRmCmd(),
	)
	return cmd
}

func newCheckAddCmd() *cobra.Command {
	var (
		kind     string
		interval time.Duration
		targets  []string
		expect   int
	)

	cmd := &cobra.Command{
		Use:     "add <name> --kind <tcp|ping|http> --interval <duration> --target <name=spec>",
		Short:   "Register a probe check",
		Example: "exocomp check add at-home --kind tcp --interval 5m --target laptop=192.168.1.50:22 --target nas=192.168.1.60:22",
		Long: "Kind tcp targets are name=ip:port — the address is the location, so " +
			"probe the home-LAN address a host is only reachable at when it is home. " +
			"Kind ping targets are name=ip (no port). Kind http targets are name=url, " +
			"with an optional --expect status; the body is never read.\n\n" +
			"Kind ping is refused on a host that cannot actually send ICMP: a " +
			"loopback probe must answer, or the add fails.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if interval <= 0 {
				return errors.New("check add: --interval must be a positive Go duration (e.g. 5m)")
			}
			if len(targets) == 0 {
				return errors.New("check add: need at least one --target")
			}
			parsed, err := store.ParseCheckTargets(kind, targets, false)
			if err != nil {
				return fmt.Errorf("check add: %w", err)
			}
			if expect != 0 && kind != store.CheckHTTP {
				return errors.New("check add: --expect applies to kind http")
			}
			if expect != 0 {
				for i := range parsed {
					parsed[i].Expect = expect
				}
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			created, err := st.CreateCheck(cmd.Context(), store.NewCheck{
				Name:      name,
				Kind:      kind,
				IntervalS: int64(interval.Seconds()),
				Targets:   parsed,
			})
			if err != nil {
				if errors.Is(err, store.ErrDuplicate) {
					return fmt.Errorf("check add: %q already exists", name)
				}
				return validationError(err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "check %s (%s) added: %d target(s) every %s\n",
				created.Name, created.Kind, len(created.Targets), interval)
			return err
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "tcp|ping|http")
	cmd.Flags().DurationVar(&interval, "interval", 0, "probe interval as a Go duration (e.g. 5m, 30s)")
	cmd.Flags().StringArrayVar(&targets, "target", nil, "name=ip:port (tcp), name=ip (ping), or name=url (http); repeatable")
	cmd.Flags().IntVar(&expect, "expect", 0, "http: required status (default: any 2xx/3xx)")
	_ = cmd.MarkFlagRequired("kind")
	_ = cmd.MarkFlagRequired("interval")
	return cmd
}

// newCheckUpdateCmd re-points a stored check in place: targets replace the
// stored set, interval replaces the cadence, kind stays whatever the row held.
// Because check_state keys on (check_id, target_name), a re-point that keeps a
// target's NAME preserves its state — the next sweep can flip the kept row and
// emit a real transition, which is what rm+add (state cascaded away) never can.
func newCheckUpdateCmd() *cobra.Command {
	var (
		interval time.Duration
		targets  []string
		kind     string
	)

	cmd := &cobra.Command{
		Use:     "update <name> --target <name=spec> [--interval <duration>]",
		Short:   "Re-point a probe check, keeping its state",
		Example: "exocomp check update at-home --interval 5m --target laptop=192.168.1.50:22",
		Long: "Replaces the check's targets (and optionally its interval) in place. " +
			"The kind is not updatable: targets keep the stored kind's shape. " +
			"A target whose NAME is kept across the update keeps its state row, so " +
			"the sweep can flip it and emit a real transition; rm+add would cascade " +
			"the state away.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if kind != "" {
				return errors.New("check update: --kind is not updatable; the stored kind is kept")
			}
			if len(targets) == 0 {
				return errors.New("check update: need at least one --target")
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			cur, err := st.GetCheck(cmd.Context(), name)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("check update: %q not found", name)
				}
				return err
			}
			parsed, err := store.ParseCheckTargets(cur.Kind, targets, false)
			if err != nil {
				return fmt.Errorf("check update: %w", err)
			}
			// A non-positive --interval means the flag was omitted; keep the
			// stored cadence, the same sentinel convention as the timeout
			// flags (a 0s probe interval is meaningless anyway).
			intervalS := int64(interval.Seconds())
			if intervalS <= 0 {
				intervalS = cur.IntervalS
			}

			updated, err := st.UpdateCheck(cmd.Context(), name, parsed, intervalS)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("check update: %q not found", name)
				}
				return validationError(err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "check %s updated: %d target(s) every %s\n",
				updated.Name, len(updated.Targets), store.CheckIntervalLabel(updated.IntervalS))
			return err
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "not updatable; the stored kind is kept")
	cmd.Flags().DurationVar(&interval, "interval", 0, "probe interval as a Go duration (e.g. 5m, 30s)")
	cmd.Flags().StringArrayVar(&targets, "target", nil, "name=ip:port (tcp), name=ip (ping), or name=url (http); repeatable")
	return cmd
}

func newCheckListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List probe checks",
		Example: "exocomp check list --output json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListChecks(cmd.Context(), false)
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return err
			}
			states := make(map[int64][]store.CheckState, len(rows))
			for _, c := range rows {
				s, err := st.ListCheckStates(cmd.Context(), c.ID)
				if err != nil {
					if mode == outputJSON {
						return failJSON(cmd, err)
					}
					return err
				}
				states[c.ID] = s
			}
			if mode == outputJSON {
				groups, err := st.ListNetgroups(cmd.Context())
				if err != nil {
					return failJSON(cmd, err)
				}
				netgroups, err := checkNetgroups(rows, groups)
				if err != nil {
					return failJSON(cmd, err)
				}
				return emitJSON(cmd, wire.KindCheckList, wire.NewCheckList(rows, states, netgroups))
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tKIND\tINTERVAL\tTARGETS\tSTATUS\tENABLED")
			for _, c := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					c.Name, c.Kind, store.CheckIntervalLabel(c.IntervalS),
					checkTargetsLabel(c.Targets), checkStatusesLabel(states[c.ID], c.Targets),
					enabledLabel(c.Enabled))
			}
			return w.Flush()
		},
	}
}

func newCheckRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a probe check",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.DeleteCheck(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("check rm: %q not found", args[0])
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "check %s removed\n", args[0])
			return err
		},
	}
}

func newCheckEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <name>",
		Short: "Let a probe check run again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setCheckEnabled(cmd, args[0], true)
		},
	}
}

func newCheckDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <name>",
		Short: "Stop a probe check from running, keeping its definition and state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setCheckEnabled(cmd, args[0], false)
		},
	}
}

// setCheckEnabled flips the stored flag. A running daemon's sweep reads the
// table on every pass, so the change lands on the next sweep tick.
func setCheckEnabled(cmd *cobra.Command, name string, enabled bool) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.SetCheckEnabled(cmd.Context(), name, enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("check %s: %q not found", enabledVerb(enabled), name)
		}
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "check %s %s\n", name, enabledLabel(enabled))
	return err
}

func checkTargetsLabel(targets []store.CheckTarget) string {
	parts := make([]string, len(targets))
	for i, t := range targets {
		parts[i] = t.Name
	}
	return strings.Join(parts, ",")
}

// checkStatusesLabel joins each target's last observed status in the same order
// as the TARGETS column, so the STATUS cell lines up with the targets it names.
// A target the sweep has never seen reads "unobserved".
func checkStatusesLabel(states []store.CheckState, targets []store.CheckTarget) string {
	byName := make(map[string]string, len(states))
	for _, s := range states {
		byName[s.TargetName] = s.Status
	}
	parts := make([]string, len(targets))
	for i, t := range targets {
		if s, ok := byName[t.Name]; ok {
			parts[i] = s
		} else {
			parts[i] = "unobserved"
		}
	}
	return strings.Join(parts, ",")
}

// checkNetgroups classifies every address target across the listed checks into
// its netgroup (or "away") with one netgroup read, so `check list --output
// json` projects the same location signal the checks page and the API do.
func checkNetgroups(rows []store.Check, groups []store.Netgroup) (map[string]string, error) {
	var addrs []netip.Addr
	for _, c := range rows {
		for _, t := range c.Targets {
			if t.Address == "" {
				continue
			}
			addr, err := netip.ParseAddr(t.Address)
			if err != nil {
				return nil, fmt.Errorf("check %s target %q address %q does not parse: %w",
					c.Name, t.Name, t.Address, err)
			}
			addrs = append(addrs, addr)
		}
	}
	return store.ClassifyNetgroups(addrs, groups), nil
}
