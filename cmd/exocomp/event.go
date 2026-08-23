package main

import (
	"errors"
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newEventCmd)
}

func newEventCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "event",
		Short: "Inspect received events and why they did or did not fire",
		Long: "Every received event is stored with its outcome, including the ones nothing " +
			"matched. \"Why didn't my rule fire?\" is a query here, not an archaeology " +
			"exercise.",
	}
	cmd.AddCommand(newEventListCmd(), newEventExplainCmd())
	return cmd
}

func newEventListCmd() *cobra.Command {
	var (
		limit   int
		source  string
		outcome string
	)

	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List received events, newest first",
		Example: "exocomp event list --outcome fired --output json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			filter := store.EventFilter{Source: source, Limit: limit}
			if outcome != "" {
				if !knownOutcome(outcome) {
					return fmt.Errorf("event list: --outcome must be one of %s",
						joinOutcomes())
				}
				filter.Outcomes = []store.EventOutcome{store.EventOutcome(outcome)}
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListEvents(cmd.Context(), filter)
			if err != nil {
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindEventList, wire.NewEventList(rows))
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no events")
				return nil
			}

			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tRECEIVED\tSOURCE\tOUTCOME\tRULE\tJOB\tREASON")
			for _, e := range rows {
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
					e.ID, e.ReceivedAt.Format("2006-01-02 15:04:05Z"), e.Source,
					e.Outcome, ruleLabel(e.MatchedRule), lastJobIDLabel(e.JobID), e.Reason)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 25, "how many events to show (0: all)")
	cmd.Flags().StringVar(&source, "source", "", "only events from this source")
	cmd.Flags().StringVar(&outcome, "outcome", "", joinOutcomes())
	return cmd
}

func outcomes() []store.EventOutcome {
	return []store.EventOutcome{
		store.OutcomeIgnored, store.OutcomeMatched, store.OutcomeSuppressed,
		store.OutcomeGated, store.OutcomeFired, store.OutcomeError,
	}
}

func knownOutcome(s string) bool {
	for _, o := range outcomes() {
		if string(o) == s {
			return true
		}
	}
	return false
}

func joinOutcomes() string {
	out := ""
	for i, o := range outcomes() {
		if i > 0 {
			out += "|"
		}
		out += string(o)
	}
	return out
}

func ruleLabel(name string) string {
	if name == "" {
		return "-"
	}
	return name
}

func newEventExplainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain <id>",
		Short: "Show the per-condition trace that decided an event",
		Long: "The trace is read back as it was recorded, not recomputed: the ledgers it " +
			"consulted have moved on since, and a replay would answer a different question " +
			"than the one being asked.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("event explain: %q is not an event id", args[0])
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			explained, err := reactive.Explain(cmd.Context(), st, id)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("event explain: no event %d", id)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), explained.Render())
			return err
		},
	}
}
