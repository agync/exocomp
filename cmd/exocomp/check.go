package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"exocomp.dev/exocomp/internal/checkprobe"
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
		newCheckExportCmd(),
		newCheckApplyCmd(),
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

	results, err := checkprobe.Run(ctx, st, c, only)
	if err != nil {
		if errors.Is(err, checkprobe.ErrNoTarget) {
			return fmt.Errorf("check probe: %s has no target named %q", name, only)
		}
		return fmt.Errorf("check probe: %w", err)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TARGET\tSTATUS\tDURATION\tLOCATION\tDETAIL")
	for _, r := range results {
		status, detail := "UP", ""
		if !r.Up {
			status, detail = "DOWN", r.Detail
		}
		location := r.Netgroup
		if location == "" {
			location = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, status,
			(time.Duration(r.DurationMS) * time.Millisecond).String(), location, detail)
	}
	return w.Flush()
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
		aggregate  bool
		minUp      int
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
			if cmd.Flags().Changed("min-up") && !aggregate {
				return errors.New("check add: --min-up needs --aggregate")
			}
			if minUp < 0 || minUp > len(parsed) {
				return fmt.Errorf("check add: --min-up must be between 0 and %d (0 = every target)", len(parsed))
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
				Aggregate: aggregate,
				MinUp:     minUp,
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
	cmd.Flags().BoolVar(&aggregate, "aggregate", false, "emit one check-level event when enough targets are up")
	cmd.Flags().IntVar(&minUp, "min-up", 0,
		"aggregate: up targets needed for the aggregate to read up (0 = every target)")
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
		aggregate  bool
		minUp      int
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

			// Aggregate config is a separate store mutation with its own
			// threshold bound; resolve the effective values against the
			// stored row so a min-up the new target set cannot satisfy is
			// refused before anything is written.
			agg := cur.Aggregate
			if cmd.Flags().Changed("aggregate") {
				agg = aggregate
			}
			threshold := cur.MinUp
			if cmd.Flags().Changed("min-up") {
				threshold = minUp
			}
			if !agg {
				threshold = 0
			}
			if threshold < 0 || threshold > len(parsed) {
				return fmt.Errorf("check update: --min-up must be between 0 and %d (0 = every target)", len(parsed))
			}

			updated, err := st.UpdateCheck(cmd.Context(), name, parsed, intervalS)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("check update: %q not found", name)
				}
				return validationError(err)
			}
			if agg != cur.Aggregate || threshold != cur.MinUp {
				if err := st.SetCheckAggregate(cmd.Context(), name, agg, threshold); err != nil {
					return validationError(err)
				}
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
	cmd.Flags().BoolVar(&aggregate, "aggregate", false, "emit one check-level event when enough targets are up")
	cmd.Flags().IntVar(&minUp, "min-up", 0,
		"aggregate: up targets needed for the aggregate to read up (0 = every target)")
	return cmd
}

func newCheckListCmd() *cobra.Command {
	var netgroup string

	cmd := &cobra.Command{
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
				kept := make([]store.CheckState, 0, len(s))
				for _, one := range s {
					if one.TargetName == store.AggregateTargetName {
						continue
					}
					kept = append(kept, one)
				}
				states[c.ID] = kept
			}
			if netgroup != "" {
				rows = filterChecksByStoredNetgroup(rows, states, netgroup)
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindCheckList, wire.NewCheckList(rows, states))
			}

			now := time.Now().UTC()
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tKIND\tINTERVAL\tTARGETS\tSTATUS\tNETGROUP\tDURATION\tAGGREGATE\tENABLED")
			for _, c := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					c.Name, c.Kind, store.CheckIntervalLabel(c.IntervalS),
					checkTargetsLabel(c.Targets), checkStatusesLabel(states[c.ID], c.Targets),
					checkNetgroupsLabel(states[c.ID], c.Targets), checkDurationsLabel(states[c.ID], c.Targets),
					checkAggregateLabel(c), checkEnabledLabel(c, now))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&netgroup, "netgroup", "",
		"only checks with at least one target whose stored netgroup is this name")
	return cmd
}

// filterChecksByStoredNetgroup narrows the listed checks to those with at least
// one target whose already-stored check_state netgroup equals name. It reads the
// sweep's persisted classification rather than re-classifying an address, so the
// filter agrees with the NETGROUP column the same listing prints.
func filterChecksByStoredNetgroup(rows []store.Check, states map[int64][]store.CheckState, name string) []store.Check {
	kept := make([]store.Check, 0, len(rows))
	for _, c := range rows {
		for _, st := range states[c.ID] {
			if st.Netgroup == name {
				kept = append(kept, c)
				break
			}
		}
	}
	return kept
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

// checkAggregateLabel renders the AGGREGATE column: "-" when the check emits
// only per-target events, "all" when the aggregate needs every target up, and
// "min N" when N targets must be up.
func checkAggregateLabel(c store.Check) string {
	if !c.Aggregate {
		return "-"
	}
	if c.MinUp == 0 {
		return "all"
	}
	return "min " + fmt.Sprint(c.MinUp)
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

// probeConfig is the file schema `check export`/`netgroup export` emit and
// `check apply`/`netgroup apply` read: the two lists live in one document so an
// operator can keep the whole probe estate in one file. Each apply reads only
// its own list, and each export emits only its own, so the round trip is exact.
type probeConfig struct {
	Checks    []configCheck    `yaml:"checks,omitempty" json:"checks,omitempty"`
	Netgroups []configNetgroup `yaml:"netgroups,omitempty" json:"netgroups,omitempty"`
}

// configCheck is one check's declarative form. Enabled is a pointer so a
// hand-written file that omits it starts the check enabled (the store's own
// default), while an export always writes it explicitly to stay round-trip
// exact. Interval is a Go duration string, the same shape the flags take.
type configCheck struct {
	Name      string         `yaml:"name" json:"name"`
	Kind      string         `yaml:"kind" json:"kind"`
	Interval  string         `yaml:"interval" json:"interval"`
	Enabled   *bool          `yaml:"enabled" json:"enabled"`
	Aggregate bool           `yaml:"aggregate" json:"aggregate"`
	MinUp     int            `yaml:"min_up" json:"min_up"`
	Targets   []configTarget `yaml:"targets" json:"targets"`
}

// configTarget is one probe target. Only the fields the check's kind uses are
// emitted, so a tcp target carries address+port and a dns target dns_name,
// exactly as the target's probe spec defines.
type configTarget struct {
	Name       string  `yaml:"name" json:"name"`
	Address    string  `yaml:"address,omitempty" json:"address,omitempty"`
	Port       int     `yaml:"port,omitempty" json:"port,omitempty"`
	URL        string  `yaml:"url,omitempty" json:"url,omitempty"`
	Expect     int     `yaml:"expect,omitempty" json:"expect,omitempty"`
	DNSName    string  `yaml:"dns_name,omitempty" json:"dns_name,omitempty"`
	ExpectAddr string  `yaml:"expect_addr,omitempty" json:"expect_addr,omitempty"`
	MinDays    int     `yaml:"min_days,omitempty" json:"min_days,omitempty"`
	Op         string  `yaml:"op,omitempty" json:"op,omitempty"`
	Value      float64 `yaml:"value,omitempty" json:"value,omitempty"`
}

// configNetgroup is one netgroup's declarative form: its includes plain and its
// excludes as the bare CIDRs the `!` in a prefix list marks.
type configNetgroup struct {
	Name     string   `yaml:"name" json:"name"`
	Includes []string `yaml:"includes" json:"includes"`
	Excludes []string `yaml:"excludes,omitempty" json:"excludes,omitempty"`
}

// probeExportMode resolves the export format. Export's natural serialization is
// YAML, so the default and "text" both mean yaml; "json" selects the machine
// shape. It is separate from outputMode because the list verbs' text table is
// not a file an apply can read back.
func probeExportMode(cmd *cobra.Command) (string, error) {
	mode := "yaml"
	if f := cmd.Flags().Lookup(outputFlag); f != nil && f.Changed {
		mode = f.Value.String()
	} else if env := strings.TrimSpace(os.Getenv(outputEnv)); env != "" {
		mode = env
	}
	switch mode {
	case "yaml", outputText:
		return "yaml", nil
	case outputJSON:
		return outputJSON, nil
	default:
		return "", fmt.Errorf("--output must be yaml or json, not %q", mode)
	}
}

// writeProbeConfig renders the document in the requested format. Struct field
// order is the file's field order, so an export is byte-stable across runs.
func writeProbeConfig(cmd *cobra.Command, mode string, doc probeConfig) error {
	out := cmd.OutOrStdout()
	if mode == outputJSON {
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

// readConfigFile reads the apply input, treating "-" as stdin so
// `export | apply -f -` works in a pipeline.
func readConfigFile(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	return os.ReadFile(path)
}

// parseProbeConfig decodes a config document strictly: a typo'd key is an
// operator mistake worth failing on, not a constraint silently dropped from the
// estate. JSON input is valid YAML, so one decoder serves both formats.
func parseProbeConfig(data []byte) (probeConfig, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc probeConfig
	if err := dec.Decode(&doc); err != nil {
		return probeConfig{}, err
	}
	return doc, nil
}

// checksToConfig projects the stored checks into the file schema in store order
// (by name), which is what makes the file diff cleanly.
func checksToConfig(rows []store.Check) []configCheck {
	out := make([]configCheck, 0, len(rows))
	for _, c := range rows {
		enabled := c.Enabled
		targets := make([]configTarget, 0, len(c.Targets))
		for _, t := range c.Targets {
			targets = append(targets, configTarget{
				Name: t.Name, Address: t.Address, Port: t.Port, URL: t.URL,
				Expect: t.Expect, DNSName: t.DNSName, ExpectAddr: t.ExpectAddr,
				MinDays: t.MinDays, Op: t.Op, Value: t.Value,
			})
		}
		out = append(out, configCheck{
			Name: c.Name, Kind: c.Kind,
			Interval:  (time.Duration(c.IntervalS) * time.Second).String(),
			Enabled:   &enabled,
			Aggregate: c.Aggregate, MinUp: c.MinUp,
			Targets: targets,
		})
	}
	return out
}

func newCheckExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "export",
		Short:   "Print every check as an applyable config file",
		Example: "exocomp check export > checks.yaml",
		Args:    cobra.NoArgs,
		Long: "Prints the declarative form of every check, ordered by name, as YAML " +
			"(default) or JSON. The output is exactly what `check apply -f` reads, so a " +
			"live estate can be committed to git and a fresh box converged from the file.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := probeExportMode(cmd)
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
				return err
			}
			return writeProbeConfig(cmd, mode, probeConfig{Checks: checksToConfig(rows)})
		},
	}
}

func newCheckApplyCmd() *cobra.Command {
	var (
		file      string
		prune     bool
		yesReally bool
	)

	cmd := &cobra.Command{
		Use:     "apply -f <file>",
		Short:   "Create or update checks from a config file",
		Example: "exocomp check apply -f checks.yaml",
		Long: "Reconciles the stored checks to the file, idempotent by name: a name " +
			"absent from the store is created, a present one is updated in place, and an " +
			"identical one is left untouched and reported unchanged. A check's kind is not " +
			"updatable, so a file that changes a stored check's kind is refused by name " +
			"rather than silently recreated; remove and re-add it deliberately. `-f -` " +
			"reads stdin.\n\n--prune deletes every check absent from the file and needs " +
			"--yes-really (or the global --assume-yes): a whole-estate delete is not " +
			"something to discover from an empty grep.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if prune {
				if err := confirmDestructive(cmd, yesReally,
					"check apply --prune (deletes every check absent from the file)",
					"--yes-really"); err != nil {
					return err
				}
			}
			data, err := readConfigFile(cmd, file)
			if err != nil {
				return fmt.Errorf("check apply: %w", err)
			}
			doc, err := parseProbeConfig(data)
			if err != nil {
				return fmt.Errorf("check apply: %w", err)
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			return applyChecks(cmd, st, doc.Checks, prune)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "config file to apply (- for stdin)")
	cmd.Flags().BoolVar(&prune, "prune", false, "delete every check absent from the file")
	cmd.Flags().BoolVar(&yesReally, "yes-really", false, "confirm --prune")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// plannedCheck is one file row after parsing, before any store write.
type plannedCheck struct {
	row       configCheck
	targets   []store.CheckTarget
	intervalS int64
	enabled   bool
}

// applyChecks reconciles the file's checks against the store. Every row is
// parsed and its kind compared to the stored one before the first write, so a
// file that would refuse halfway leaves nothing half-applied. Target shape
// validation is left to the store verbs (CreateCheck/UpdateCheck), which is the
// same gate `check add` passes through.
func applyChecks(cmd *cobra.Command, st *store.Store, rows []configCheck, prune bool) error {
	ctx := cmd.Context()
	existing, err := st.ListChecks(ctx, false)
	if err != nil {
		return err
	}
	byName := make(map[string]store.Check, len(existing))
	for _, c := range existing {
		byName[c.Name] = c
	}

	plans := make([]plannedCheck, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if seen[row.Name] {
			return fmt.Errorf("check apply: %q appears twice in the file", row.Name)
		}
		seen[row.Name] = true
		if cur, ok := byName[row.Name]; ok && cur.Kind != row.Kind {
			return fmt.Errorf("check apply: %q is kind %s; the file says %s, and a kind is not updatable — remove it and add it again",
				row.Name, cur.Kind, row.Kind)
		}
		d, err := time.ParseDuration(row.Interval)
		if err != nil || d <= 0 {
			return fmt.Errorf("check apply: %s: interval %q must be a positive Go duration (e.g. 5m)", row.Name, row.Interval)
		}
		targets, err := configTargets(row.Kind, row.Targets)
		if err != nil {
			return fmt.Errorf("check apply: %s: %w", row.Name, err)
		}
		// Preflight the full per-kind shape too (IP literal, port range, url,
		// ping capability), so a file whose LAST row is invalid does not leave
		// its earlier rows half applied.
		if err := store.ValidateCheckTargets(row.Name, row.Kind, targets); err != nil {
			return fmt.Errorf("check apply: %s: %w", row.Name, validationError(err))
		}
		enabled := true
		if row.Enabled != nil {
			enabled = *row.Enabled
		}
		plans = append(plans, plannedCheck{row: row, targets: targets, intervalS: int64(d.Seconds()), enabled: enabled})
	}

	var created, updated, unchanged, deleted int
	for _, p := range plans {
		cur, ok := byName[p.row.Name]
		if !ok {
			if _, err := st.CreateCheck(ctx, store.NewCheck{
				Name: p.row.Name, Kind: p.row.Kind, IntervalS: p.intervalS,
				Targets: p.targets, Aggregate: p.row.Aggregate, MinUp: p.row.MinUp,
			}); err != nil {
				return fmt.Errorf("check apply: %s: %w", p.row.Name, validationError(err))
			}
			if !p.enabled {
				if err := st.SetCheckEnabled(ctx, p.row.Name, false); err != nil {
					return fmt.Errorf("check apply: %s: %w", p.row.Name, err)
				}
			}
			created++
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "check %s: created\n", p.row.Name)
			continue
		}

		changed := false
		if !reflect.DeepEqual(cur.Targets, p.targets) || cur.IntervalS != p.intervalS {
			if _, err := st.UpdateCheck(ctx, p.row.Name, p.targets, p.intervalS); err != nil {
				return fmt.Errorf("check apply: %s: %w", p.row.Name, validationError(err))
			}
			changed = true
		}
		if cur.Aggregate != p.row.Aggregate || cur.MinUp != p.row.MinUp {
			if err := st.SetCheckAggregate(ctx, p.row.Name, p.row.Aggregate, p.row.MinUp); err != nil {
				return fmt.Errorf("check apply: %s: %w", p.row.Name, validationError(err))
			}
			changed = true
		}
		if cur.Enabled != p.enabled {
			if err := st.SetCheckEnabled(ctx, p.row.Name, p.enabled); err != nil {
				return fmt.Errorf("check apply: %s: %w", p.row.Name, err)
			}
			changed = true
		}
		if changed {
			updated++
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "check %s: updated\n", p.row.Name)
		} else {
			unchanged++
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "check %s: unchanged\n", p.row.Name)
		}
	}

	if prune {
		names := make([]string, 0, len(byName))
		for name := range byName {
			if !seen[name] {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			if err := st.DeleteCheck(ctx, name); err != nil {
				return fmt.Errorf("check apply: %s: %w", name, err)
			}
			deleted++
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "check %s: deleted\n", name)
		}
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "check apply: %d created, %d updated, %d unchanged, %d deleted\n",
		created, updated, unchanged, deleted)
	return err
}

// configTargets rebuilds probe targets from the file through
// store.ParseCheckTargets, the same parse `check add` uses, then applies the
// kind-specific fields the parsed target does not carry. Reconstructing the
// name=spec form keeps the store as the one validator rather than a second
// shape check that could drift.
func configTargets(kind string, rows []configTarget) ([]store.CheckTarget, error) {
	if len(rows) == 0 {
		return nil, errors.New("needs at least one target")
	}
	specs := make([]string, 0, len(rows))
	for _, t := range rows {
		specs = append(specs, t.Name+"="+configTargetValue(kind, t))
	}
	targets, err := store.ParseCheckTargets(kind, specs, false)
	if err != nil {
		return nil, err
	}
	for i := range targets {
		targets[i].Expect = rows[i].Expect
		targets[i].MinDays = rows[i].MinDays
		targets[i].ExpectAddr = rows[i].ExpectAddr
		targets[i].Op = rows[i].Op
		targets[i].Value = rows[i].Value
	}
	return targets, nil
}

// configTargetValue renders a file target back to the value half of its
// name=spec. An unknown kind falls through to Address so ParseCheckTargets —
// not this helper — reports the unknown kind.
func configTargetValue(kind string, t configTarget) string {
	switch kind {
	case store.CheckTCP, store.CheckTLS:
		return net.JoinHostPort(t.Address, strconv.Itoa(t.Port))
	case store.CheckDNS:
		return t.DNSName
	case store.CheckHTTP, store.CheckMetric:
		return t.URL
	default:
		return t.Address
	}
}
