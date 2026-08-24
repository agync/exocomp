package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// kindApprovalDecision mirrors internal/web/api_write.go's kindDecision: the
// same wire kind for the same shape, so a decision looks identical whether it
// arrived over the HTTP API or this command. wire itself carries no write
// projections yet (another lane's seam), which is why both surfaces define
// their own copy of this shape rather than sharing an unexported one.
const kindApprovalDecision = "approval.decision"

// approvalDecisionView is the JSON body of a settled decision: which approval,
// the outcome, and the terminal notice. Field-for-field identical to the API's
// decisionView.
type approvalDecisionView struct {
	ApprovalID int64  `json:"approval_id"`
	Outcome    string `json:"outcome,omitempty"`
	Message    string `json:"message,omitempty"`
}

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
	cmd.AddCommand(newApprovalListCmd(), newApprovalLinkCmd(), newApprovalDecideCmd())
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
	var publicURL string
	cmd := &cobra.Command{
		Use:   "link <id>",
		Short: "Print the decision links for a pending approval",
		Long: "The links are single-use capabilities derived from the envelope, so they can " +
			"be reprinted after a restart and are worthless to anyone without the data dir. " +
			"Treat the output the way you would treat the recovery card.\n\n" +
			"By default the link is rooted at the daemon's recorded loopback address, which " +
			"is unreachable off-box and does not survive a restart. Pass --public-url (or set " +
			"" + approvalPublicURLEnv + ") to the SAME value `exocomp serve --public-url` was " +
			"started with, so the minted link is one an approver can actually open.",
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
			base, err := linkBaseURL(dir, publicURL)
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
	cmd.Flags().StringVar(&publicURL, "public-url", "",
		"base URL the link is rooted at (or "+approvalPublicURLEnv+"); pass the same value "+
			"`serve --public-url` used (default: the daemon's recorded loopback address)")
	return cmd
}

// newApprovalDecideCmd settles a pending approval. With --remote it POSTs the
// decision to the daemon, which derives the capability token server-side — a
// scope is not a capability link, so nothing bearer-like leaves this box either
// way. Locally it goes straight at the same store list/link already open,
// deriving the SAME capability token `approval link` prints rather than asking
// the operator to paste one back in — this is an operator-authority action at
// the data dir, not a bearer-link redemption.
func newApprovalDecideCmd() *cobra.Command {
	var approve, reject bool
	cmd := &cobra.Command{
		Use:   "decide <id> (--approve | --reject)",
		Short: "Settle a pending approval",
		Long: "Routes through the same fail-closed seam a decision link and the HTTP " +
			"decision API use (reactive.Engine.Decide): the capability token is derived " +
			"from the envelope and verified, then the TTL, then — for an approve — whether " +
			"the plan is still current. A used, expired, or raced approval settles nothing " +
			"further; this command reports that as a failure rather than a quiet no-op, so " +
			"a script cannot mistake a stale decide for a fresh one.",
		Example: "exocomp approval decide 5 --approve",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if approve == reject {
				return errors.New("approval decide: give exactly one of --approve or --reject")
			}
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("approval decide: %q is not an approval id", args[0])
			}

			decision := reactive.DecisionReject
			if approve {
				decision = reactive.DecisionApprove
			}
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				return runRemoteDecide(cmd, rc, id, decision)
			}

			dir, err := dataDir()
			if err != nil {
				return err
			}
			// A decide can release the gated job into queued, which is running
			// jobs' entry point (queue.New below), so it opens through
			// openJobStore like every other one: a concurrency key an abandoned
			// process left held must clear before this release, not after.
			st, err := openJobStore(cmd.Context(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			approval, err := st.GetApproval(cmd.Context(), id)
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return fmt.Errorf("approval decide: %w", err)
			}

			env, err := secrets.Open(dir)
			if err != nil {
				return fmt.Errorf("approval decide: open envelope (run `exocomp init` first): %w", err)
			}
			defer func() { _ = env.Close() }()

			token, _, err := reactive.ApprovalToken(env, approval.JobID)
			if err != nil {
				return fmt.Errorf("approval decide: %w", err)
			}

			// The queue this engine drives is enqueue-only (like workspace
			// plan/apply's daemonEnqueuer): a decide settles the row and, for an
			// approve, moves it from awaiting_approval to queued. It never runs a
			// job itself — a running `exocomp serve` claims the released row on its
			// own poll, same as any other CLI-side enqueue.
			gate := queue.New(st, enqueueOnly{}, queue.Options{Workers: 1})
			eng, err := reactive.New(reactive.Options{Store: st, Queue: gate, Deriver: env})
			if err != nil {
				return fmt.Errorf("approval decide: %w", err)
			}

			svc := service.New(st, dir, nil)
			res, err := svc.DecideApproval(cmd.Context(), eng, service.DecideInput{
				ApprovalID: id,
				Decision:   decision,
				Token:      token,
				Via:        "cli",
			})
			if err != nil {
				// The one case Decide itself refuses is a stale plan (ErrStalePlan);
				// wire.ErrorFor maps it to CodeStalePlan. Everything else that settles
				// nothing new (already-decided, expired) comes back with a nil error
				// and is handled in the switch below.
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return fmt.Errorf("approval decide: %w", err)
			}

			d := res.Decision
			switch d.Outcome {
			case reactive.DecisionOutcomeApproved, reactive.DecisionOutcomeRejected:
				if mode == outputJSON {
					return emitJSON(cmd, kindApprovalDecision, approvalDecisionView{
						ApprovalID: d.Approval.ID, Outcome: d.Outcome, Message: d.Message,
					})
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout(), d.Message)
				return err
			default:
				// Settled (already decided) or expired: the invariant this command
				// shares with the phone link and the HTTP API is that a used/expired/
				// raced decision is idempotent, never a second release — so nothing
				// this call asked for happened, and it fails closed rather than
				// exiting 0 on a no-op.
				werr := fmt.Errorf("approval decide: %s", d.Message)
				if mode == outputJSON {
					cmd.SilenceErrors = true
					_ = wire.WriteErrorValue(cmd.OutOrStdout(), wire.NewError(wire.CodeConflict, d.Message))
					return werr
				}
				return werr
			}
		},
	}
	cmd.Flags().BoolVar(&approve, "approve", false, "approve the job and release it to the queue")
	cmd.Flags().BoolVar(&reject, "reject", false, "reject the job; nothing is applied")
	return cmd
}

// approvalPublicURLEnv is the env-var counterpart to `approval link
// --public-url`, named to match `serve --public-url` (which reads no env
// itself — see linkBaseURL). Exporting it once alongside the flag means every
// later `approval link` in that environment picks up the same value without
// repeating it.
const approvalPublicURLEnv = "EXOCOMP_PUBLIC_URL"

// linkBaseURL is what a decision link is rooted at. `serve --public-url` sets
// this same base for the PUSH decision-link path (engine.SetBaseURL in
// serve.go), but that value lives only in the running daemon's memory
// (internal/reactive.Engine.baseURL) — it is never persisted, so a separate
// `approval link` invocation cannot read it back on its own. override (the
// --public-url flag, or EXOCOMP_PUBLIC_URL) is how an operator supplies the
// same value here; only when neither is set does this fall back to the
// recorded loopback address, which is unreachable off-box and does not
// survive a restart.
func linkBaseURL(dir, override string) (string, error) {
	if v := strings.TrimSpace(override); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv(approvalPublicURLEnv)); v != "" {
		return v, nil
	}
	return daemonBaseURL(dir)
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
