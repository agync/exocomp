package main

import (
	"errors"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newSilenceCmd)
}

func newSilenceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "silence",
		Short: "Swallow matching events until a deadline",
		Long: "A silence has no never-expires form: --for is mandatory at every scope. A " +
			"mute the operator forgets is how a homelab stops noticing it is broken.",
	}
	cmd.AddCommand(newSilenceAddCmd(), newSilenceListCmd(), newSilenceRmCmd())
	return cmd
}

func newSilenceAddCmd() *cobra.Command {
	var (
		alert     string
		host      string
		global    bool
		yesReally bool
		reason    string
		duration  time.Duration
	)

	cmd := &cobra.Command{
		Use:   "add",
		Short: "Silence an alert, a host, or everything",
		Long: "Scope ladder, narrowest first: --alert, --host, --global. The global scope " +
			"is the reactive kill switch and needs --yes-really: it swallows every event " +
			"from every source. It does not stop running jobs finishing, and it does not " +
			"stop schedules firing — it stops events becoming new jobs.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scope, value, err := silenceScope(alert, host, global)
			if err != nil {
				return err
			}
			if scope == store.ScopeGlobal && !yesReally {
				return errors.New("silence add: --global swallows every event from every " +
					"source; pass --yes-really to confirm (running jobs still finish and " +
					"schedules still fire)")
			}
			if duration <= 0 {
				return errors.New("silence add: --for is mandatory and must be positive")
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			now := time.Now().UTC()
			created, err := st.CreateSilence(cmd.Context(), store.Silence{
				ScopeKind:  scope,
				ScopeValue: value,
				Reason:     reason,
				CreatedAt:  now,
				ExpiresAt:  now.Add(duration),
				CreatedVia: "cli",
			})
			if err != nil {
				return fmt.Errorf("silence add: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "silence %d: %s until %s\n",
				created.ID, scopeLabel(created), created.ExpiresAt.Format("2006-01-02 15:04:05Z"))
			return err
		},
	}
	cmd.Flags().StringVar(&alert, "alert", "", "silence one alert name")
	cmd.Flags().StringVar(&host, "host", "", "silence one host")
	cmd.Flags().BoolVar(&global, "global", false, "silence everything (the kill switch)")
	cmd.Flags().BoolVar(&yesReally, "yes-really", false, "confirm a global silence")
	cmd.Flags().StringVar(&reason, "reason", "", "why, for the banner and the audit")
	cmd.Flags().DurationVar(&duration, "for", 0, "how long it lasts (required)")
	_ = cmd.MarkFlagRequired("for")
	return cmd
}

// silenceScope resolves the scope flags to exactly one scope.
func silenceScope(alert, host string, global bool) (kind, value string, err error) {
	chosen := 0
	for _, set := range []bool{alert != "", host != "", global} {
		if set {
			chosen++
		}
	}
	switch {
	case chosen == 0:
		return "", "", errors.New("silence add: pick a scope: --alert, --host, or --global")
	case chosen > 1:
		return "", "", errors.New("silence add: --alert, --host, and --global are exclusive")
	case alert != "":
		return store.ScopeAlert, alert, nil
	case host != "":
		return store.ScopeHost, host, nil
	default:
		return store.ScopeGlobal, "", nil
	}
}

func scopeLabel(s store.Silence) string {
	if s.ScopeKind == store.ScopeGlobal {
		return "global pause"
	}
	return s.ScopeKind + " " + s.ScopeValue
}

func newSilenceListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active silences",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListSilences(cmd.Context(), !all)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				if all {
					_, _ = fmt.Fprintln(out, "no silences")
				} else {
					_, _ = fmt.Fprintln(out, "no active silences")
				}
				return nil
			}

			now := time.Now().UTC()
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tSCOPE\tEXPIRES\tLEFT\tVIA\tREASON")
			for _, s := range rows {
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
					s.ID, scopeLabel(s), s.ExpiresAt.Format("2006-01-02 15:04:05Z"),
					leftLabel(s.ExpiresAt, now), viaLabel(s.CreatedVia), s.Reason)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include silences that have already expired")
	return cmd
}

func leftLabel(expires, now time.Time) string {
	if !now.Before(expires) {
		return "expired"
	}
	return expires.Sub(now).Round(time.Second).String()
}

func viaLabel(via string) string {
	if via == "" {
		return "-"
	}
	return via
}

func newSilenceRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Lift a silence before it expires",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("silence rm: %q is not a silence id", args[0])
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.DeleteSilence(cmd.Context(), id); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("silence rm: no silence %d", id)
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "silence %d lifted\n", id)
			return err
		},
	}
}
