package main

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
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

			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				return runRemoteMuteAdd(cmd, rc, scope, value, reason, duration)
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
			printMuteAdded(cmd, created.ID, created.ScopeKind, created.ScopeValue, created.ExpiresAt)
			return nil
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

func muteLabel(kind, value string) string {
	if kind == store.ScopeGlobal {
		return "global"
	}
	return kind + " " + value
}

func printMuteAdded(cmd *cobra.Command, id int64, kind, value string, expires time.Time) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "mute %d: %s until %s\n",
		id, muteLabel(kind, value), expires.Format("2006-01-02 15:04:05Z"))
}

func printMuteLifted(cmd *cobra.Command, id int64) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "mute %d lifted\n", id)
}

func newMuteListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List active mutes",
		Example: "exocomp mute list --output json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			var views []wire.MuteView
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				if views, err = rc.ListMutes(cmd.Context(), all); err != nil {
					return remoteFail(cmd, mode, err)
				}
			} else {
				st, err := openStore()
				if err != nil {
					if mode == outputJSON {
						return failJSON(cmd, err)
					}
					return err
				}
				defer func() { _ = st.Close() }()
				rows, err := st.ListNotifyMutes(cmd.Context(), !all)
				if err != nil {
					if mode == outputJSON {
						return failJSON(cmd, err)
					}
					return err
				}
				views = wire.NewMuteList(rows)
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindMuteList, views)
			}
			rows := make([]holdRow, 0, len(views))
			for _, v := range views {
				rows = append(rows, holdRow{v.ID, muteLabel(v.ScopeKind, v.ScopeValue), wireTime(v.ExpiresAt),
					v.CreatedVia, v.Reason})
			}
			return printHoldTable(cmd, rows, "mute", all)
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

			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				return runRemoteMuteRm(cmd, rc, id)
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
			printMuteLifted(cmd, id)
			return nil
		},
	}
}
