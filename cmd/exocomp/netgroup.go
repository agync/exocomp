package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newNetgroupCmd)
}

func newNetgroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "netgroup",
		Short: "Manage named CIDR sets for classifying addresses",
		Long: "A netgroup names a set of CIDRs with the operator's own vocabulary, " +
			"so a reactive rule classifies an address — a webhook peer or a check " +
			"target — as `home` or `office` instead of hardcoding the prefixes. " +
			"Every received webhook is stamped with the peer's `peer_ip` and the " +
			"most specific group containing it, or `away` when none does " +
			"(docs/design/check-triggered-runs.md §8).",
	}
	cmd.AddCommand(
		newNetgroupAddCmd(), newNetgroupUpdateCmd(), newNetgroupListCmd(),
		newNetgroupShowCmd(), newNetgroupUsageCmd(), newNetgroupRmCmd(),
		newNetgroupExportCmd(), newNetgroupApplyCmd(),
	)
	return cmd
}

func newNetgroupAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "add <name> <cidr[,cidr...]>",
		Short:   "Register a named CIDR set",
		Example: "exocomp netgroup add home 192.168.0.0/16,!192.168.5.0/24",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := store.ParsePrefixList(args[1])
			if err != nil {
				return fmt.Errorf("netgroup add: %w", err)
			}
			if len(list.Includes) == 0 {
				return errors.New("netgroup add: need at least one include CIDR")
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			created, err := st.CreateNetgroup(cmd.Context(), store.NewNetgroup{
				Name:     args[0],
				Prefixes: list.Includes,
				Excludes: list.Excludes,
			})
			if err != nil {
				if errors.Is(err, store.ErrDuplicate) {
					return fmt.Errorf("netgroup add: %q already exists", args[0])
				}
				return fmt.Errorf("netgroup add: %w", validationError(err))
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "netgroup %s added: %s\n",
				created.Name, netgroupPrefixesLabel(created.Prefixes, created.Excludes))
			return err
		},
	}
}

func newNetgroupUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "update <name> <cidr[,cidr...]>",
		Short:   "Replace a named CIDR set in place",
		Example: "exocomp netgroup update home 192.168.0.0/16,!192.168.5.0/24",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := store.ParsePrefixList(args[1])
			if err != nil {
				return fmt.Errorf("netgroup update: %w", err)
			}
			if len(list.Includes) == 0 {
				return errors.New("netgroup update: need at least one include CIDR")
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			updated, err := st.UpdateNetgroup(cmd.Context(), args[0], list.Includes, list.Excludes)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("netgroup update: %q not found", args[0])
				}
				return fmt.Errorf("netgroup update: %w", validationError(err))
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "netgroup %s updated: %s\n",
				updated.Name, netgroupPrefixesLabel(updated.Prefixes, updated.Excludes))
			return err
		},
	}
}

func newNetgroupListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List named CIDR sets",
		Example: "exocomp netgroup list --output json",
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

			rows, err := st.ListNetgroups(cmd.Context())
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindNetgroupList, wire.NewNetgroupList(rows))
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tPREFIXES")
			for _, g := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%s\n", g.Name, netgroupPrefixesLabel(g.Prefixes, g.Excludes))
			}
			return w.Flush()
		},
	}
}

func newNetgroupShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "show <addr>",
		Short:   "Classify one address",
		Example: "exocomp netgroup show 192.168.5.10",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, err := netip.ParseAddr(args[0])
			if err != nil {
				return fmt.Errorf("netgroup show: %q is not an IP address", args[0])
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			groups, err := st.ListNetgroups(cmd.Context())
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ADDRESS\tNETGROUP\tPREFIX")
			if name, prefix, ok := store.ClassifyAddr(addr, groups); ok {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", addr.Unmap().String(), name, prefix.String())
			} else {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", addr.Unmap().String(), store.NetgroupAway, "-")
			}
			return w.Flush()
		},
	}
}

// netgroupUsageEventScan bounds how many recent events the usage report reads.
// It is a report over what was observed, not an audit of all history, so a
// fixed window keeps it stable and cheap on a large event table.
const netgroupUsageEventScan = 1000

// netgroupUsageRow is one group's observed members: the check targets whose
// stored state classified into it, and the webhook peers whose events did.
type netgroupUsageRow struct {
	Targets []string
	Peers   []string
}

func newNetgroupUsageCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "usage [<name>]",
		Short:   "Show where checks and webhook peers classify",
		Example: "exocomp netgroup usage home",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, order, err := netgroupUsage(cmd.Context(), st)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				name := args[0]
				if _, ok := rows[name]; !ok {
					return fmt.Errorf("netgroup usage: %q is not registered and no observation names it", name)
				}
				order = []string{name}
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NETGROUP\tCHECKS\tPEERS")
			for _, name := range order {
				r := rows[name]
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", name, joinOrDash(r.Targets), joinOrDash(r.Peers))
			}
			return w.Flush()
		},
	}
}

// netgroupUsage reads the stored classifications — check_state.netgroup for
// target state and each webhook event's peer_netgroup label for peers — into
// per-group rows. Nothing is classified fresh: the report shows exactly what
// the pipeline observed. Rows are deduped and sorted, registered groups are
// listed by name, any observed-but-unregistered name follows, and "away" comes
// last.
func netgroupUsage(ctx context.Context, st *store.Store) (map[string]*netgroupUsageRow, []string, error) {
	rows := map[string]*netgroupUsageRow{}

	list, err := st.ListNetgroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	var order []string
	registered := make([]string, 0, len(list))
	for _, g := range list {
		registered = append(registered, g.Name)
		if _, ok := rows[g.Name]; !ok {
			rows[g.Name] = &netgroupUsageRow{}
		}
	}

	checks, err := st.ListChecks(ctx, false)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range checks {
		states, err := st.ListCheckStates(ctx, c.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, s := range states {
			if s.Netgroup == "" {
				continue
			}
			r := usageRow(rows, s.Netgroup)
			r.Targets = append(r.Targets, c.Name+":"+s.TargetName)
		}
	}

	events, err := st.ListEvents(ctx, store.EventFilter{Limit: netgroupUsageEventScan})
	if err != nil {
		return nil, nil, err
	}
	for _, e := range events {
		var body struct {
			Labels map[string]string `json:"labels"`
		}
		if err := json.Unmarshal([]byte(e.Normalized), &body); err != nil {
			continue
		}
		group := body.Labels["peer_netgroup"]
		if group == "" {
			continue
		}
		r := usageRow(rows, group)
		if ip := body.Labels["peer_ip"]; ip != "" {
			r.Peers = append(r.Peers, ip)
		}
	}

	order = append(order, registered...)
	var extras []string
	for name := range rows {
		if !slices.Contains(registered, name) && name != store.NetgroupAway {
			extras = append(extras, name)
		}
	}
	sort.Strings(extras)
	order = append(order, extras...)
	if _, ok := rows[store.NetgroupAway]; ok {
		order = append(order, store.NetgroupAway)
	}
	for _, name := range order {
		r := rows[name]
		sort.Strings(r.Targets)
		r.Targets = slices.Compact(r.Targets)
		sort.Strings(r.Peers)
		r.Peers = slices.Compact(r.Peers)
	}
	return rows, order, nil
}

func usageRow(rows map[string]*netgroupUsageRow, name string) *netgroupUsageRow {
	if r, ok := rows[name]; ok {
		return r
	}
	r := &netgroupUsageRow{}
	rows[name] = r
	return r
}

func newNetgroupRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a named CIDR set",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.DeleteNetgroup(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("netgroup rm: %q not found", args[0])
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "netgroup %s removed\n", args[0])
			return err
		},
	}
}

// netgroupPrefixesLabel renders a group's set the way both the add/update
// argument and the list table read it: includes plain, excludes `!`-prefixed.
func netgroupPrefixesLabel(includes, excludes []netip.Prefix) string {
	parts := make([]string, 0, len(includes)+len(excludes))
	for _, p := range includes {
		parts = append(parts, p.String())
	}
	for _, p := range excludes {
		parts = append(parts, "!"+p.String())
	}
	return strings.Join(parts, ",")
}

func joinOrDash(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}

// netgroupsToConfig projects the stored netgroups into the file schema in store
// order (by name).
func netgroupsToConfig(rows []store.Netgroup) []configNetgroup {
	out := make([]configNetgroup, 0, len(rows))
	for _, g := range rows {
		includes := make([]string, len(g.Prefixes))
		for i, p := range g.Prefixes {
			includes[i] = p.String()
		}
		var excludes []string
		for _, p := range g.Excludes {
			excludes = append(excludes, p.String())
		}
		out = append(out, configNetgroup{Name: g.Name, Includes: includes, Excludes: excludes})
	}
	return out
}

func newNetgroupExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "export",
		Short:   "Print every netgroup as an applyable config file",
		Example: "exocomp netgroup export > netgroups.yaml",
		Args:    cobra.NoArgs,
		Long: "Prints the declarative form of every netgroup, ordered by name, as YAML " +
			"(default) or JSON. The output is exactly what `netgroup apply -f` reads, so " +
			"the classification vocabulary lives in git with the rest of the estate.",
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

			rows, err := st.ListNetgroups(cmd.Context())
			if err != nil {
				return err
			}
			return writeProbeConfig(cmd, mode, probeConfig{Netgroups: netgroupsToConfig(rows)})
		},
	}
}

func newNetgroupApplyCmd() *cobra.Command {
	var (
		file      string
		prune     bool
		yesReally bool
	)

	cmd := &cobra.Command{
		Use:     "apply -f <file>",
		Short:   "Create or update netgroups from a config file",
		Example: "exocomp netgroup apply -f netgroups.yaml",
		Long: "Reconciles the stored netgroups to the file, idempotent by name: a name " +
			"absent from the store is created, a present one has its set replaced in " +
			"place, and an identical one is reported unchanged. `-f -` reads stdin.\n\n" +
			"--prune deletes every netgroup absent from the file and needs --yes-really " +
			"(or the global --assume-yes).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if prune {
				if err := confirmDestructive(cmd, yesReally,
					"netgroup apply --prune (deletes every netgroup absent from the file)",
					"--yes-really"); err != nil {
					return err
				}
			}
			data, err := readConfigFile(cmd, file)
			if err != nil {
				return fmt.Errorf("netgroup apply: %w", err)
			}
			doc, err := parseProbeConfig(data)
			if err != nil {
				return fmt.Errorf("netgroup apply: %w", err)
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			return applyNetgroups(cmd, st, doc.Netgroups, prune)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "config file to apply (- for stdin)")
	cmd.Flags().BoolVar(&prune, "prune", false, "delete every netgroup absent from the file")
	cmd.Flags().BoolVar(&yesReally, "yes-really", false, "confirm --prune")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// applyNetgroups reconciles the file's netgroups against the store over
// store.ParsePrefixList, the same parse `netgroup add` uses, so the write-time
// validation is never duplicated.
func applyNetgroups(cmd *cobra.Command, st *store.Store, rows []configNetgroup, prune bool) error {
	ctx := cmd.Context()
	existing, err := st.ListNetgroups(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]store.Netgroup, len(existing))
	for _, g := range existing {
		byName[g.Name] = g
	}

	type plannedNetgroup struct {
		row  configNetgroup
		list store.PrefixList
	}
	plans := make([]plannedNetgroup, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if seen[row.Name] {
			return fmt.Errorf("netgroup apply: %q appears twice in the file", row.Name)
		}
		seen[row.Name] = true
		list, err := store.ParsePrefixList(netgroupSpec(row))
		if err != nil {
			return fmt.Errorf("netgroup apply: %s: %w", row.Name, err)
		}
		if len(list.Includes) == 0 {
			return fmt.Errorf("netgroup apply: %s: needs at least one include CIDR", row.Name)
		}
		plans = append(plans, plannedNetgroup{row: row, list: list})
	}

	var created, updated, unchanged, deleted int
	for _, p := range plans {
		cur, ok := byName[p.row.Name]
		if !ok {
			if _, err := st.CreateNetgroup(ctx, store.NewNetgroup{
				Name: p.row.Name, Prefixes: p.list.Includes, Excludes: p.list.Excludes,
			}); err != nil {
				return fmt.Errorf("netgroup apply: %s: %w", p.row.Name, validationError(err))
			}
			created++
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "netgroup %s: created\n", p.row.Name)
			continue
		}
		if reflect.DeepEqual(cur.Prefixes, p.list.Includes) && reflect.DeepEqual(cur.Excludes, p.list.Excludes) {
			unchanged++
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "netgroup %s: unchanged\n", p.row.Name)
			continue
		}
		if _, err := st.UpdateNetgroup(ctx, p.row.Name, p.list.Includes, p.list.Excludes); err != nil {
			return fmt.Errorf("netgroup apply: %s: %w", p.row.Name, validationError(err))
		}
		updated++
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "netgroup %s: updated\n", p.row.Name)
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
			if err := st.DeleteNetgroup(ctx, name); err != nil {
				return fmt.Errorf("netgroup apply: %s: %w", name, err)
			}
			deleted++
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "netgroup %s: deleted\n", name)
		}
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "netgroup apply: %d created, %d updated, %d unchanged, %d deleted\n",
		created, updated, unchanged, deleted)
	return err
}

// netgroupSpec recombines a file group's includes and excludes into the single
// comma-separated list store.ParsePrefixList reads, `!`-marking the excludes.
func netgroupSpec(row configNetgroup) string {
	parts := make([]string, 0, len(row.Includes)+len(row.Excludes))
	parts = append(parts, row.Includes...)
	for _, e := range row.Excludes {
		parts = append(parts, "!"+e)
	}
	return strings.Join(parts, ",")
}
