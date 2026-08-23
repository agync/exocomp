package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newApprovalCmd)
}

func newApprovalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approval",
		Short: "Inspect the jobs waiting on a human",
		Long: "An approval is a queue state: the job it names sits in awaiting_approval and " +
			"no worker can claim it until the decision arrives. The TTL fails closed — an " +
			"unanswered approval is rejected, never eventually run.",
	}
	cmd.AddCommand(newApprovalListCmd(), newApprovalLinkCmd())
	return cmd
}

func newApprovalListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List approvals waiting on a decision",
		Example: "exocomp approval list --output json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				rows, err := rc.ListApprovals(cmd.Context(), all)
				if err != nil {
					return remoteFail(cmd, mode, err)
				}
				return renderRemoteApprovals(cmd, mode, rows)
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListApprovals(cmd.Context(), !all)
			if err != nil {
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindApprovalList, wire.NewApprovalList(rows))
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "nothing is waiting on you")
				return nil
			}

			now := time.Now().UTC()
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tSTATE\tJOB\tRUNBOOK\tWORKSPACE\tPLAN SERIAL\tTTL\tVIA")
			for _, a := range rows {
				runbook := "-"
				if job, err := st.GetJob(cmd.Context(), a.JobID); err == nil {
					runbook = job.Runbook
				}
				_, _ = fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
					a.ID, a.State, a.JobID, runbook, workspaceLabel(a.Workspace),
					planSerialLabel(a), reactive.ApprovalDeadline(a, now), viaLabel(a.DecidedVia))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include approvals that have already been decided")
	return cmd
}

func workspaceLabel(name string) string {
	if name == "" {
		return "-"
	}
	return name
}

func planSerialLabel(a store.Approval) string {
	if a.Workspace == "" {
		return "-"
	}
	return strconv.FormatInt(a.PlanSerial, 10)
}

// newApprovalLinkCmd re-derives the decision links for one approval. They are
// bearer capabilities: printing one is handing it out, which is why this is a
// separate command rather than a column in `approval list`.
func newApprovalLinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "link <id>",
		Short: "Print the decision links for a pending approval",
		Long: "The links are single-use capabilities derived from the envelope, so they can " +
			"be reprinted after a restart and are worthless to anyone without the data dir. " +
			"Treat the output the way you would treat the recovery card.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("approval link: %q is not an approval id", args[0])
			}

			dir, err := dataDir()
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			approval, err := st.GetApproval(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("approval link: %w", err)
			}
			if approval.State != store.ApprovalPending {
				return fmt.Errorf("approval link: approval %d is %s; there is nothing left to decide",
					id, approval.State)
			}

			env, err := secrets.Open(dir)
			if err != nil {
				return fmt.Errorf("approval link: open envelope: %w", err)
			}
			defer func() { _ = env.Close() }()

			token, _, err := reactive.ApprovalToken(env, approval.JobID)
			if err != nil {
				return err
			}
			base, err := daemonBaseURL(dir)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "approve  %s\n",
				reactive.DecisionURL(base, approval.ID, reactive.DecisionApprove, token))
			_, _ = fmt.Fprintf(out, "reject   %s\n",
				reactive.DecisionURL(base, approval.ID, reactive.DecisionReject, token))
			_, _ = fmt.Fprintf(out, "expires  %s\n",
				approval.ExpiresAt.Format("2006-01-02 15:04:05Z"))
			return nil
		},
	}
}

// daemonBaseURL is the address the running daemon recorded. Without one the
// links have nowhere to point, and saying so beats printing a capability
// against a host that is not listening.
func daemonBaseURL(dir string) (string, error) {
	addr, err := api.ReadAddress(filepath.Join(dir, api.AddressFile))
	if err != nil {
		return "", err
	}
	if addr == "" {
		return "", fmt.Errorf("no daemon address recorded; start `exocomp serve` first")
	}
	return "http://" + addr, nil
}
