package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
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
