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
	registerCommand(newMuteCmd)
}

func newMuteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mute",
		Short: "Hold notifications without stopping the jobs they announce",
		Long: "A mute is not a silence. A silence swallows an event before it becomes a " +
			"job (pause); a mute lets the job run and only holds the message that would " +
			"have announced it. Like a silence, --for is mandatory: a mute the operator " +
			"forgets is how a phone goes quiet the week the backups start failing.",
	}
	cmd.AddCommand(newMuteAddCmd(), newMuteListCmd(), newMuteRmCmd())
	return cmd
}

func newMuteAddCmd() *cobra.Command {
	var (
		alert    string
		host     string
		global   bool
		reason   string
		duration time.Duration
	)

	cmd := &cobra.Command{
		Use:   "add",
		Short: "Mute an alert, a host, or every notification",
		Long: "Scope ladder, narrowest first: --alert, --host, --global. Muting does not " +
			"stop a single job, schedule, or reactive action — it only holds the phone. " +
			"Everything is loud again when --for expires.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			scope, value, err := muteScope(alert, host, global)
			if err != nil {
				return err
			}
			if duration <= 0 {
				return errors.New("mute add: --for is mandatory and must be positive")
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			now := time.Now().UTC()
			created, err := st.CreateNotifyMute(cmd.Context(), store.NotifyMute{
				ScopeKind:  scope,
				ScopeValue: value,
				Reason:     reason,
				CreatedAt:  now,
				ExpiresAt:  now.Add(duration),
				CreatedVia: "cli",
			})
			if err != nil {
				return fmt.Errorf("mute add: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "mute %d: %s until %s\n",
				created.ID, muteLabel(created), created.ExpiresAt.Format("2006-01-02 15:04:05Z"))
			return err
		},
	}
	cmd.Flags().StringVar(&alert, "alert", "", "mute one alert name")
	cmd.Flags().StringVar(&host, "host", "", "mute one host")
	cmd.Flags().BoolVar(&global, "global", false, "mute every notification")
	cmd.Flags().StringVar(&reason, "reason", "", "why, for the banner and the audit")
	cmd.Flags().DurationVar(&duration, "for", 0, "how long it lasts (required)")
	_ = cmd.MarkFlagRequired("for")
	return cmd
}

// muteScope resolves the scope flags to exactly one scope.
func muteScope(alert, host string, global bool) (kind, value string, err error) {
	chosen := 0
	for _, set := range []bool{alert != "", host != "", global} {
		if set {
			chosen++
		}
	}
	switch {
	case chosen == 0:
		return "", "", errors.New("mute add: pick a scope: --alert, --host, or --global")
	case chosen > 1:
		return "", "", errors.New("mute add: --alert, --host, and --global are exclusive")
	case alert != "":
		return store.ScopeAlert, alert, nil
	case host != "":
		return store.ScopeHost, host, nil
	default:
		return store.ScopeGlobal, "", nil
	}
}

func muteLabel(m store.NotifyMute) string {
	if m.ScopeKind == store.ScopeGlobal {
		return "global"
	}
	return m.ScopeKind + " " + m.ScopeValue
}

func newMuteListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active mutes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListNotifyMutes(cmd.Context(), !all)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				if all {
					_, _ = fmt.Fprintln(out, "no mutes")
				} else {
					_, _ = fmt.Fprintln(out, "no active mutes")
				}
				return nil
			}

			now := time.Now().UTC()
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tSCOPE\tEXPIRES\tLEFT\tVIA\tREASON")
			for _, m := range rows {
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
					m.ID, muteLabel(m), m.ExpiresAt.Format("2006-01-02 15:04:05Z"),
					leftLabel(m.ExpiresAt, now), viaLabel(m.CreatedVia), m.Reason)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include mutes that have already expired")
	return cmd
}

func newMuteRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Unmute before the deadline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("mute rm: %q is not a mute id", args[0])
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.DeleteNotifyMute(cmd.Context(), id); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("mute rm: no mute %d", id)
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "mute %d lifted\n", id)
			return err
		},
	}
}
