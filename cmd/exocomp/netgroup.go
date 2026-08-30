package main

import (
	"errors"
	"fmt"
	"net/netip"
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
	cmd.AddCommand(newNetgroupAddCmd(), newNetgroupListCmd(), newNetgroupRmCmd())
	return cmd
}

func newNetgroupAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "add <name> <cidr[,cidr...]>",
		Short:   "Register a named CIDR set",
		Example: "exocomp netgroup add home 192.168.0.0/16,10.0.0.0/8",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			prefixes, err := parsePrefixList(args[1])
			if err != nil {
				return fmt.Errorf("netgroup add: %w", err)
			}
			if len(prefixes) == 0 {
				return errors.New("netgroup add: need at least one CIDR")
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			created, err := st.CreateNetgroup(cmd.Context(), store.NewNetgroup{
				Name:     args[0],
				Prefixes: prefixes,
			})
			if err != nil {
				if errors.Is(err, store.ErrDuplicate) {
					return fmt.Errorf("netgroup add: %q already exists", args[0])
				}
				return fmt.Errorf("netgroup add: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "netgroup %s added: %s\n",
				created.Name, joinPrefixes(created.Prefixes))
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
				_, _ = fmt.Fprintf(w, "%s\t%s\n", g.Name, joinPrefixes(g.Prefixes))
			}
			return w.Flush()
		},
	}
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

// parsePrefixList splits a comma-separated CIDR argument and parses every
// member. An empty member is refused too: in the middle of a hand-typed list
// it is a double comma, a typo that should be caught here rather than stored.
func parsePrefixList(spec string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("an empty CIDR (double comma) in the list")
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR: %w", part, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func joinPrefixes(prefixes []netip.Prefix) string {
	parts := make([]string, len(prefixes))
	for i, p := range prefixes {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}
