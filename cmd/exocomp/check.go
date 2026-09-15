package main

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/probe"
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
		newCheckProbeCmd(),
		newCheckTestCmd(),
		newCheckListCmd(),
		newCheckEnableCmd(),
		newCheckDisableCmd(),
		newCheckSnoozeCmd(),
		newCheckUnsnoozeCmd(),
		newCheckRmCmd(),
	)
	return cmd
}

// checkProbeTimeout matches the sweep's bound (reactive.checks probeTimeout):
// an on-demand probe answers with the same deadline the sweep uses, so what the
// operator sees here is what the next sweep will record.
const checkProbeTimeout = 5 * time.Second

// newCheckProbeCmd runs a stored check's probes immediately, without the
// daemon, and writes nothing. It is how a check is debugged without waiting an
// interval: the result, the duration, and the location read exactly as the
// sweep will read them.
func newCheckProbeCmd() *cobra.Command {
	var target string

	cmd := &cobra.Command{
		Use:     "probe <name> [--target <target>]",
		Short:   "Run a check's probes now, writing nothing",
		Example: "exocomp check probe at-home",
		Long: "Runs each target's probe immediately with the same options the sweep " +
			"uses and prints the result, duration, and location. It writes no state " +
			"and emits no event, so it is safe to run against a live check; it needs " +
			"no running daemon. --target narrows to one target by name.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheckProbe(cmd, args[0], target)
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "probe only this target by name")
	return cmd
}

func runCheckProbe(cmd *cobra.Command, name, only string) error {
	ctx := cmd.Context()
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	c, err := st.GetCheck(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("check probe: %q not found", name)
		}
		return err
	}
	states, err := st.ListCheckStates(ctx, c.ID)
	if err != nil {
		return err
	}
	stored := make(map[string]string, len(states))
	for _, s := range states {
		stored[s.TargetName] = s.Netgroup
	}
	groups, err := st.ListNetgroups(ctx)
	if err != nil {
		return err
	}

	targets := c.Targets
	if only != "" {
		targets = nil
		for _, t := range c.Targets {
			if t.Name == only {
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("check probe: %s has no target named %q", name, only)
		}
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TARGET\tSTATUS\tDURATION\tLOCATION\tDETAIL")
	for _, t := range targets {
		spec, err := store.SpecForTarget(c.Kind, t)
		if err != nil {
			return fmt.Errorf("check probe %s: target %q: %w", name, t.Name, err)
		}
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("check probe %s: target %q: %w", name, t.Name, err)
		}
		res := probe.Run(ctx, spec, probe.Options{Timeout: checkProbeTimeout, AllowLoopback: false})
		status, detail := "UP", ""
		if !res.Up {
			status, detail = "DOWN", res.Detail
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.Name, status,
			res.Duration.Round(time.Millisecond), probeLocationLabel(spec, stored[t.Name], groups), detail)
	}
	return w.Flush()
}

// probeLocationLabel names where a target was probed: the sweep's stored
// classification when it has observed the target, else a live classification of
// the address the spec carries. A kind with no address literal (dns, metric, and
// a tls hostname) has no location and reads "-".
func probeLocationLabel(spec probe.Spec, stored string, groups []store.Netgroup) string {
	if stored != "" {
		return stored
	}
	addr, ok := spec.Location()
	if !ok {
		return "-"
	}
	if ng, ok := store.ClassifyNetgroups([]netip.Addr{addr}, groups)[addr.Unmap().String()]; ok {
		return ng
	}
	return "-"
}

// newCheckTestCmd validates and runs a one-off probe set without storing a
// check: the anti-footgun before `check add`. It reaches the runner directly,
// so no row and no state exist to write.
func newCheckTestCmd() *cobra.Command {
	var (
		kind       string
		targets    []string
		minDays    int
		expectAddr string
		op         string
		value      float64
	)

	cmd := &cobra.Command{
		Use:     "test --kind <kind> --target <name=spec>",
		Short:   "Validate and run a one-off probe set without storing it",
		Example: "exocomp check test --kind tls --target web=web.lan:443 --min-days 21",
		Long: "Runs each target once with the same validation and options `check add` " +
			"would apply, and prints the result. Nothing is stored: it is the dry run " +
			"that catches a wrong target or a bad threshold before the check exists.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(targets) == 0 {
				return errors.New("check test: need at least one --target")
			}
			parsed, err := store.ParseCheckTargets(kind, targets, false)
			if err != nil {
				return fmt.Errorf("check test: %w", err)
			}
			if err := applyKindFlags(cmd, kind, parsed, 0, minDays, expectAddr, op, value); err != nil {
				return fmt.Errorf("check test: %w", err)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "TARGET\tSTATUS\tDURATION\tDETAIL")
			for _, t := range parsed {
				spec, err := store.SpecForTarget(kind, t)
				if err != nil {
					return fmt.Errorf("check test: target %q: %w", t.Name, err)
				}
				if err := spec.Validate(); err != nil {
					return fmt.Errorf("check test: target %q: %w", t.Name, err)
				}
				res := probe.Run(cmd.Context(), spec, probe.Options{Timeout: checkProbeTimeout, AllowLoopback: false})
				status := "UP"
				if !res.Up {
					status = "DOWN"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, status,
					res.Duration.Round(time.Millisecond), res.Detail)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "tcp|ping|http|dns|tls|metric")
	cmd.Flags().StringArrayVar(&targets, "target", nil, "name=spec; repeatable")
	cmd.Flags().IntVar(&minDays, "min-days", 0, "tls: require the leaf cert to be at least this many days from expiry")
	cmd.Flags().StringVar(&expectAddr, "expect-addr", "", "dns: require the name to resolve to this address")
	cmd.Flags().StringVar(&op, "op", "", "metric: comparison operator (< <= > >= == !=)")
	cmd.Flags().Float64Var(&value, "value", 0, "metric: value the operator compares the body against")
	_ = cmd.MarkFlagRequired("kind")
	return cmd
}

func newCheckAddCmd() *cobra.Command {
	var (
		kind       string
		interval   time.Duration
		targets    []string
		expect     int
		minDays    int
		expectAddr string
		op         string
		value      float64
	)

	cmd := &cobra.Command{
		Use:     "add <name> --kind <tcp|ping|http|dns|tls|metric> --interval <duration> --target <name=spec>",
		Short:   "Register a probe check",
		Example: "exocomp check add at-home --kind tcp --interval 5m --target laptop=192.168.1.50:22 --target nas=192.168.1.60:22",
		Long: "Kind tcp targets are name=ip:port — the address is the location, so " +
			"probe the home-LAN address a host is only reachable at when it is home. " +
			"Kind ping targets are name=ip (no port). Kind http targets are name=url, " +
			"with an optional --expect status; the body is never read. Kind dns " +
			"targets are name=hostname, with an optional --expect-addr. Kind tls " +
			"targets are name=host:port, with an optional --min-days expiry floor. " +
			"Kind metric targets are name=url, with the required --op and --value.\n\n" +
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
			if err := applyKindFlags(cmd, kind, parsed, expect, minDays, expectAddr, op, value); err != nil {
				return fmt.Errorf("check add: %w", err)
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
	cmd.Flags().StringVar(&kind, "kind", "", "tcp|ping|http|dns|tls|metric")
	cmd.Flags().DurationVar(&interval, "interval", 0, "probe interval as a Go duration (e.g. 5m, 30s)")
	cmd.Flags().StringArrayVar(&targets, "target", nil, "name=ip:port (tcp/tls), name=ip (ping), name=hostname (dns), or name=url (http/metric); repeatable")
	cmd.Flags().IntVar(&expect, "expect", 0, "http: required status (default: any 2xx/3xx)")
	cmd.Flags().IntVar(&minDays, "min-days", 0, "tls: require the leaf cert to be at least this many days from expiry")
	cmd.Flags().StringVar(&expectAddr, "expect-addr", "", "dns: require the name to resolve to this address")
	cmd.Flags().StringVar(&op, "op", "", "metric: comparison operator (< <= > >= == !=)")
	cmd.Flags().Float64Var(&value, "value", 0, "metric: value the operator compares the body against")
	_ = cmd.MarkFlagRequired("kind")
	_ = cmd.MarkFlagRequired("interval")
	return cmd
}

// applyKindFlags applies the kind-specific target fields after parsing, the one
// place `check add`, `check update`, and `check test` share. A flag that does
// not belong to the check's kind is refused rather than silently dropped — the
// contract --expect has always had — and a metric probe, which cannot decide
// without one, requires an explicit --op and --value.
func applyKindFlags(cmd *cobra.Command, kind string, targets []store.CheckTarget, expect, minDays int, expectAddr, op string, value float64) error {
	if expect != 0 && kind != store.CheckHTTP {
		return errors.New("--expect applies to kind http")
	}
	if cmd.Flags().Changed("min-days") && kind != store.CheckTLS {
		return errors.New("--min-days applies to kind tls")
	}
	if cmd.Flags().Changed("expect-addr") && kind != store.CheckDNS {
		return errors.New("--expect-addr applies to kind dns")
	}
	if cmd.Flags().Changed("op") && kind != store.CheckMetric {
		return errors.New("--op applies to kind metric")
	}
	if cmd.Flags().Changed("value") && kind != store.CheckMetric {
		return errors.New("--value applies to kind metric")
	}
	if kind == store.CheckMetric && !cmd.Flags().Changed("op") {
		return errors.New("--op is required for kind metric")
	}
	if kind == store.CheckMetric && !cmd.Flags().Changed("value") {
		return errors.New("--value is required for kind metric")
	}
	for i := range targets {
		targets[i].Expect = expect
		targets[i].MinDays = minDays
		targets[i].ExpectAddr = expectAddr
		targets[i].Op = op
		targets[i].Value = value
	}
	return nil
}

// newCheckUpdateCmd re-points a stored check in place: targets replace the
// stored set, interval replaces the cadence, kind stays whatever the row held.
// Because check_state keys on (check_id, target_name), a re-point that keeps a
// target's NAME preserves its state — the next sweep can flip the kept row and
// emit a real transition, which is what rm+add (state cascaded away) never can.
func newCheckUpdateCmd() *cobra.Command {
	var (
		interval   time.Duration
		targets    []string
		kind       string
		minDays    int
		expectAddr string
		op         string
		value      float64
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
			if err := applyKindFlags(cmd, cur.Kind, parsed, 0, minDays, expectAddr, op, value); err != nil {
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
	cmd.Flags().StringArrayVar(&targets, "target", nil, "name=ip:port (tcp/tls), name=ip (ping), name=hostname (dns), or name=url (http/metric); repeatable")
	cmd.Flags().IntVar(&minDays, "min-days", 0, "tls: require the leaf cert to be at least this many days from expiry")
	cmd.Flags().StringVar(&expectAddr, "expect-addr", "", "dns: require the name to resolve to this address")
	cmd.Flags().StringVar(&op, "op", "", "metric: comparison operator (< <= > >= == !=)")
	cmd.Flags().Float64Var(&value, "value", 0, "metric: value the operator compares the body against")
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
				return emitJSON(cmd, wire.KindCheckList, wire.NewCheckList(rows, states))
			}

			now := time.Now().UTC()
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tKIND\tINTERVAL\tTARGETS\tSTATUS\tNETGROUP\tDURATION\tENABLED")
			for _, c := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					c.Name, c.Kind, store.CheckIntervalLabel(c.IntervalS),
					checkTargetsLabel(c.Targets), checkStatusesLabel(states[c.ID], c.Targets),
					checkNetgroupsLabel(states[c.ID], c.Targets), checkDurationsLabel(states[c.ID], c.Targets),
					checkEnabledLabel(c, now))
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

// newCheckSnoozeCmd mutes a check's transitions until a deadline. Like a
// silence, --for is mandatory: a snooze the operator forgets is how a check
// stops noticing it is broken. Probing and state application continue, so the
// hysteresis is current when the snooze lapses.
func newCheckSnoozeCmd() *cobra.Command {
	var duration time.Duration

	cmd := &cobra.Command{
		Use:     "snooze <name> --for <duration>",
		Short:   "Stop a check emitting transitions until a deadline",
		Example: "exocomp check snooze at-home --for 4h",
		Long: "A snooze keeps probing and advancing the check's state but suppresses " +
			"transition emission until it expires. There is no never-expires form: " +
			"--for is mandatory.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if duration <= 0 {
				return errors.New("check snooze: --for is mandatory and must be positive")
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			until := time.Now().UTC().Add(duration)
			if err := st.SetCheckSnooze(cmd.Context(), name, until); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("check snooze: %q not found", name)
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "check %s snoozed until %s\n",
				name, until.Format("2006-01-02 15:04:05Z"))
			return err
		},
	}
	cmd.Flags().DurationVar(&duration, "for", 0, "how long the snooze lasts (required)")
	_ = cmd.MarkFlagRequired("for")
	return cmd
}

func newCheckUnsnoozeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unsnooze <name>",
		Short: "Lift a check snooze before it expires",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.SetCheckSnooze(cmd.Context(), args[0], time.Time{}); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("check unsnooze: %q not found", args[0])
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "check %s unsnoozed\n", args[0])
			return err
		},
	}
}

func checkTargetsLabel(targets []store.CheckTarget) string {
	parts := make([]string, len(targets))
	for i, t := range targets {
		parts[i] = t.Name
	}
	return strings.Join(parts, ",")
}

// checkNetgroupsLabel joins each target's stored netgroup classification in the
// same order as the TARGETS column. An unobserved target and an http target
// (which has no address) read "-".
func checkNetgroupsLabel(states []store.CheckState, targets []store.CheckTarget) string {
	byName := make(map[string]string, len(states))
	for _, s := range states {
		byName[s.TargetName] = s.Netgroup
	}
	parts := make([]string, len(targets))
	for i, t := range targets {
		if ng, ok := byName[t.Name]; ok && ng != "" {
			parts[i] = ng
		} else {
			parts[i] = "-"
		}
	}
	return strings.Join(parts, ",")
}

// checkDurationsLabel joins each target's last probe duration in the same order
// as the TARGETS column. A target the sweep has never seen reads "-".
func checkDurationsLabel(states []store.CheckState, targets []store.CheckTarget) string {
	byName := make(map[string]int64, len(states))
	for _, s := range states {
		byName[s.TargetName] = s.LastDurationMS
	}
	parts := make([]string, len(targets))
	for i, t := range targets {
		ms, ok := byName[t.Name]
		if !ok {
			parts[i] = "-"
			continue
		}
		parts[i] = (time.Duration(ms) * time.Millisecond).String()
	}
	return strings.Join(parts, ",")
}

// checkEnabledLabel renders the ENABLED column, naming an active snooze and its
// remaining time in place of a bare "enabled" so a held check never reads as
// running normally.
func checkEnabledLabel(c store.Check, now time.Time) string {
	if c.Enabled && !c.SnoozedUntil.IsZero() && c.SnoozedUntil.After(now) {
		return "snoozed " + c.SnoozedUntil.Sub(now).Round(time.Second).String() + " left"
	}
	return enabledLabel(c.Enabled)
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
