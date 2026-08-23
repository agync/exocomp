package main

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newRuleCmd)
}

func newRuleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rule",
		Short: "Manage the rules that turn events into jobs",
		Long: "A rule is a CEL expression over a normalized event and the action a match " +
			"runs, bounded by a cooldown and a daily cap. Guardrails are enforced when the " +
			"action reaches the queue, so a rule cannot opt out of them.",
	}
	cmd.AddCommand(
		newRuleAddCmd(),
		newRuleListCmd(),
		newRuleEnableCmd(),
		newRuleDisableCmd(),
		newRuleRmCmd(),
	)
	return cmd
}

func newRuleAddCmd() *cobra.Command {
	var (
		expr            string
		action          string
		source          string
		workspace       string
		cooldown        time.Duration
		dailyCap        int
		requireApproval bool
		timeout         time.Duration
	)

	cmd := &cobra.Command{
		Use:   "add <name> [-- <argv...>]",
		Short: "Add a rule, refusing an expression that will not compile",
		Long: "The expression is checked against the same environment the engine evaluates " +
			"it in, so a rule that would never run is refused here rather than logged on " +
			"every event. Action shell runs argv (everything after --); tofu-plan and " +
			"tofu-apply act on --workspace; notify sends a push and creates no job. " +
			"tofu-apply is destructive and is approval-gated whatever --require-approval says.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, argv := args[0], args[1:]

			var ruleAction store.RuleAction
			switch action {
			case store.ActionShell:
				if len(argv) == 0 {
					return errors.New("rule add: action shell needs argv after --")
				}
				ruleAction.Argv = argv
			case store.ActionTofuPlan, store.ActionTofuApply:
				if workspace == "" {
					return fmt.Errorf("rule add: action %s needs --workspace", action)
				}
				if len(argv) != 0 {
					return fmt.Errorf("rule add: action %s takes no argv", action)
				}
				ruleAction.Workspace = workspace
			case store.ActionNotify:
				if len(argv) != 0 {
					return errors.New("rule add: action notify takes no argv")
				}
			default:
				return fmt.Errorf("rule add: --action must be one of %s",
					strings.Join(actionKinds(), ", "))
			}
			if cooldown < 0 {
				return errors.New("rule add: --cooldown cannot be negative")
			}
			if dailyCap < 0 {
				return errors.New("rule add: --daily-cap cannot be negative")
			}
			ruleAction.Timeout = store.Duration(timeout)

			if _, err := reactive.Compile(expr); err != nil {
				return fmt.Errorf("rule add: %w", err)
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			created, err := st.CreateRule(cmd.Context(), store.NewRule{
				Name:            name,
				SourceFilter:    source,
				CELExpr:         expr,
				ActionKind:      action,
				Action:          ruleAction,
				CooldownS:       int(cooldown / time.Second),
				DailyCap:        dailyCap,
				RequireApproval: requireApproval,
			})
			if err != nil {
				if errors.Is(err, store.ErrDuplicate) {
					return fmt.Errorf("rule add: %q already exists", name)
				}
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "rule %s (%s) added\n", created.Name, created.ActionKind)
			if reactive.Destructive(created.ActionKind) && !created.RequireApproval {
				_, _ = fmt.Fprintf(out,
					"note: %s is destructive, so every match is approval-gated regardless\n",
					created.ActionKind)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&expr, "expr", "", "CEL expression over `event` (required)")
	cmd.Flags().StringVar(&action, "action", "", strings.Join(actionKinds(), "|"))
	cmd.Flags().StringVar(&source, "source", "",
		"only evaluate events from this source (empty: every source)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "tofu actions: the workspace to act on")
	cmd.Flags().DurationVar(&cooldown, "cooldown", 0, "refuse to fire again within this long")
	cmd.Flags().IntVar(&dailyCap, "daily-cap", 0,
		"fires per 24h before the rule disables itself (0: no cap)")
	cmd.Flags().BoolVar(&requireApproval, "require-approval", false,
		"gate every match behind an approval")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "kill the job after this long (0: no limit)")
	_ = cmd.MarkFlagRequired("expr")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

func actionKinds() []string {
	return []string{store.ActionShell, store.ActionTofuPlan, store.ActionTofuApply, store.ActionNotify}
}

func newRuleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List rules with their guardrails",
		Example: "exocomp rule list --output json",
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

			rows, err := st.ListRules(cmd.Context(), false)
			if err != nil {
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindRuleList, wire.NewRuleList(rows))
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no rules")
				return nil
			}

			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tSTATE\tSOURCE\tACTION\tCOOLDOWN\tCAP\tAPPROVAL\tEXPR")
			for _, r := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Name, enabledLabel(r.Enabled), sourceLabel(r.SourceFilter),
					actionLabel(r), cooldownLabel(r.CooldownS), capLabel(r.DailyCap),
					approvalLabel(r), r.CELExpr)
			}
			return w.Flush()
		},
	}
}

func sourceLabel(filter string) string {
	if filter == "" {
		return "any"
	}
	return filter
}

func actionLabel(r store.Rule) string {
	switch r.ActionKind {
	case store.ActionShell:
		return r.ActionKind + " " + strings.Join(r.Action.Argv, " ")
	case store.ActionTofuPlan, store.ActionTofuApply:
		return r.ActionKind + " " + r.Action.Workspace
	default:
		return r.ActionKind
	}
}

func cooldownLabel(seconds int) string {
	if seconds <= 0 {
		return "-"
	}
	return (time.Duration(seconds) * time.Second).String()
}

func capLabel(limit int) string {
	if limit <= 0 {
		return "-"
	}
	return fmt.Sprint(limit)
}

// approvalLabel says why a rule is gated, because "destructive" is a property
// of the action rather than a flag the operator set.
func approvalLabel(r store.Rule) string {
	switch {
	case reactive.Destructive(r.ActionKind):
		return "always (destructive)"
	case r.RequireApproval:
		return "required"
	default:
		return "-"
	}
}

func newRuleEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <name>",
		Short: "Let a rule match again",
		Long: "Re-enable a rule you disabled. A daily cap no longer disables a rule — it " +
			"throttles (the action is withheld, the rule stays armed and pages once, and " +
			"the budget recovers as the 24h window clears), so this is for a manual disable.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setRuleEnabled(cmd, args[0], true)
		},
	}
}

func newRuleDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <name>",
		Short: "Stop a rule from matching, keeping its definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setRuleEnabled(cmd, args[0], false)
		},
	}
}

func setRuleEnabled(cmd *cobra.Command, name string, enabled bool) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.SetRuleEnabled(cmd.Context(), name, enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("rule %s: %q not found", enabledVerb(enabled), name)
		}
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "rule %s %s\n", name, enabledLabel(enabled))
	return err
}

func newRuleRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a rule and its fire ledger",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			if err := st.DeleteRule(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("rule rm: %q not found", args[0])
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "rule %s removed\n", args[0])
			return err
		},
	}
}
