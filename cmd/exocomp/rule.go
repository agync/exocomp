package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
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
		repoSource      string
		playbook        string
		limit           string
		tags            string
		check           bool
		diff            bool
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
			"tofu-apply act on --workspace; playbook runs the named playbook from a " +
			"registered source's checkout (--repo-source) under ansible; notify sends a " +
			"push and creates no job. tofu-apply is destructive and is approval-gated " +
			"whatever --require-approval says.\n\n" +
			"A playbook rule is a plain, vault-less, inventory-discovery play for v1: it " +
			"carries no run-secrets or vault binding, and --limit is required because an " +
			"un-limited rule would run every host the inventory names.",
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
			case store.ActionPlaybook:
				if repoSource == "" {
					return errors.New("rule add: action playbook needs --repo-source")
				}
				if playbook == "" {
					return errors.New("rule add: action playbook needs --playbook")
				}
				if limit == "" {
					return errors.New("rule add: action playbook needs --limit (default-deny)")
				}
				if len(argv) != 0 {
					return errors.New("rule add: action playbook takes no argv")
				}
				ruleAction.Source = repoSource
				ruleAction.Playbook = playbook
				ruleAction.Limit = limit
				if tags != "" {
					ruleAction.Tags = strings.Split(tags, ",")
				}
				ruleAction.Check = check
				ruleAction.Diff = diff
			case store.ActionNotify:
				if len(argv) != 0 {
					return errors.New("rule add: action notify takes no argv")
				}
				if requireApproval {
					return errors.New("rule add: action notify creates no job, so --require-approval has nothing to gate")
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

			if action == store.ActionPlaybook {
				if err := validatePlaybookRule(cmd.Context(), st, repoSource, playbook, limit, tags, check, diff); err != nil {
					return err
				}
			}

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
	cmd.Flags().StringVar(&repoSource, "repo-source", "",
		"playbook action: the registered source whose checkout holds the playbook "+
			"(not --source, which is the event-source filter)")
	cmd.Flags().StringVar(&playbook, "playbook", "",
		"playbook action: path to the playbook, relative to the --repo-source checkout")
	cmd.Flags().StringVar(&limit, "limit", "",
		"playbook action: ansible --limit host pattern (required: default-deny)")
	cmd.Flags().StringVar(&tags, "tags", "",
		"playbook action: ansible --tags, comma-separated")
	cmd.Flags().BoolVar(&check, "check", false, "playbook action: ansible --check dry run")
	cmd.Flags().BoolVar(&diff, "diff", false, "playbook action: ansible --diff")
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
	return []string{store.ActionShell, store.ActionTofuPlan, store.ActionTofuApply,
		store.ActionPlaybook, store.ActionNotify}
}

// validatePlaybookRule refuses a playbook-action rule that would never run: the
// source must exist with a checkout on disk, the playbook path must be relative
// and confined to that checkout, the source must not be hooked (a rule playbook
// is a plain run — the override-wins resolution covers both the checkout's own
// .exocomp/hooks.yml and the data-dir override), and the limit/tags must be
// shape-valid for ansible. All of it re-validated by the builder when the rule
// fires; refusing here keeps a rule that can never fire out of `rule list`.
func validatePlaybookRule(ctx context.Context, st *store.Store, repoSource, playbook, limit, tags string, check, diff bool) error {
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("rule add: resolve data dir: %w", err)
	}
	hooked, _, err := service.New(st, dir, nil).ResolveSourcePlaybook(ctx,
		hooksOverridePath(dir, repoSource), repoSource, playbook)
	if err != nil {
		return fmt.Errorf("rule add: %w", err)
	}
	if hooked {
		return fmt.Errorf("rule add: source %q has hooks; a rule playbook is a plain run", repoSource)
	}
	if _, _, err := service.AnsiblePassthrough(limit, tags, "", check, diff, nil); err != nil {
		return fmt.Errorf("rule add: %w", err)
	}
	return nil
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
					r.Name, enabledLabel(r.Enabled), reactive.SourceLabel(r.SourceFilter),
					reactive.ActionLabel(r), reactive.CooldownLabel(r.CooldownS),
					reactive.CapLabel(r.DailyCap), reactive.ApprovalLabel(r), r.CELExpr)
			}
			return w.Flush()
		},
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
