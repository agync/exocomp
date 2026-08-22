package main

import (
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newPolicyCmd)
}

func newPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Manage the CEL policies that gate a workspace's sealed plans",
		Long: "A plan policy is a CEL expression over a workspace's `tofu show -json` plan, " +
			"evaluated when a plan is sealed. A deny policy that matches makes the sealed plan " +
			"unconsumable, so a later apply of it refuses naming the policy; a require_approval " +
			"policy routes that apply through the approval gate. The expression is checked to " +
			"compile here, so a policy that could never decide is refused rather than stored.",
	}
	cmd.AddCommand(newPolicyAddCmd(), newPolicyListCmd(), newPolicyRmCmd())
	return cmd
}

func newPolicyAddCmd() *cobra.Command {
	var (
		workspace string
		expr      string
		action    string
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a plan policy, refusing an expression that will not compile",
		Long: "The expression is compiled against the same environment the seal-time engine " +
			"evaluates it in — one variable, `plan`, bound to the plan-show JSON — so a policy " +
			"that would error on every plan is refused now rather than silently failing a seal. " +
			"--action is deny (the sealed plan becomes unconsumable) or require_approval (the " +
			"apply is gated).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if workspace == "" {
				return errors.New("policy add: --workspace is required")
			}
			switch action {
			case store.PolicyDeny, store.PolicyRequireApproval:
			default:
				return fmt.Errorf("policy add: --action must be %s or %s",
					store.PolicyDeny, store.PolicyRequireApproval)
			}
			// Refuse an expression that cannot compile or does not decide a bool:
			// a policy that cannot decide must never reach a seal, where its
			// failure would otherwise have to choose between blocking every plan
			// and waving every plan through.
			if _, err := policy.Compile(expr); err != nil {
				return fmt.Errorf("policy add: %w", err)
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			created, err := st.CreatePlanPolicy(cmd.Context(), store.PlanPolicy{
				Name:      name,
				Workspace: workspace,
				Expr:      expr,
				Action:    action,
			})
			if errors.Is(err, store.ErrDuplicate) {
				return fmt.Errorf("policy add: %q already exists", name)
			}
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "policy %s (%s) on workspace %s added\n",
				created.Name, created.Action, created.Workspace)
			return nil
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "the workspace whose plans this policy gates (required)")
	cmd.Flags().StringVar(&expr, "expr", "", "CEL expression over `plan` (required)")
	cmd.Flags().StringVar(&action, "action", "",
		store.PolicyDeny+"|"+store.PolicyRequireApproval)
	_ = cmd.MarkFlagRequired("workspace")
	_ = cmd.MarkFlagRequired("expr")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

func newPolicyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List plan policies with their workspace and action",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListPlanPolicies(cmd.Context(), "", false)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no plan policies")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tWORKSPACE\tACTION\tENABLED\tEXPR")
			for _, p := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\n",
					p.Name, p.Workspace, p.Action, p.Enabled, p.Expr)
			}
			return w.Flush()
		},
	}
}

func newPolicyRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a plan policy by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.DeletePlanPolicy(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("policy rm: no policy %q", args[0])
				}
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "policy %s removed\n", args[0])
			return nil
		},
	}
}
