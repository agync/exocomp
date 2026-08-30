package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
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
			parsed, err := parseCheckTargets(kind, targets)
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
				return err
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

// parseCheckTargets splits each name=spec --target by kind. The spec after the
// '=' is an IP literal (tcp/ping) or a URL (http); a tcp target carries its
// port after the last ':'.
func parseCheckTargets(kind string, targets []string) ([]store.CheckTarget, error) {
	out := make([]store.CheckTarget, 0, len(targets))
	for _, spec := range targets {
		name, value, ok := strings.Cut(spec, "=")
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("target %q must be name=spec", spec)
		}
		t := store.CheckTarget{Name: name}
		switch kind {
		case store.CheckTCP:
			host, port, err := net.SplitHostPort(value)
			if err != nil {
				return nil, fmt.Errorf("target %q: a tcp target must be ip:port", spec)
			}
			p, err := strconv.Atoi(port)
			if err != nil {
				return nil, fmt.Errorf("target %q: port %q is not a number", spec, port)
			}
			t.Address = host
			t.Port = p
		case store.CheckPing:
			t.Address = value
		case store.CheckHTTP:
			t.URL = value
		default:
			return nil, fmt.Errorf("--kind must be %s, %s or %s",
				store.CheckTCP, store.CheckPing, store.CheckHTTP)
		}
		out = append(out, t)
	}
	return out, nil
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
				return emitJSON(cmd, wire.KindCheckList, wire.NewCheckList(rows, states))
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tKIND\tINTERVAL\tTARGETS\tENABLED")
			for _, c := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					c.Name, c.Kind, checkIntervalLabel(c.IntervalS),
					checkTargetsLabel(c.Targets), enabledLabel(c.Enabled))
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

func checkIntervalLabel(intervalS int64) string {
	if intervalS <= 0 {
		return "-"
	}
	return (time.Duration(intervalS) * time.Second).String()
}

func checkTargetsLabel(targets []store.CheckTarget) string {
	parts := make([]string, len(targets))
	for i, t := range targets {
		parts[i] = t.Name
	}
	return strings.Join(parts, ",")
}
