package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/store"
)

// This file holds the remote paths of the schedule and rule verbs. Each sends
// the verb to the selected daemon, which checks it with the same service verb
// a local run calls, and prints what the local verb prints.

func runRemoteScheduleAdd(cmd *cobra.Command, rc *client.Client, n store.NewSchedule) error {
	// The daemon takes a play's playbook and the check-mode pair, nothing else
	// after it; say so before sending rather than relay its refusal.
	if n.Kind == sched.KindPlay && len(n.Args.Argv) > 1 {
		for _, a := range n.Args.Argv[1:] {
			if a != "--check" && a != "--diff" {
				return fmt.Errorf("schedule add: with a remote selected a play schedule takes its playbook "+
					"and --check; set %s on the daemon's host with --local", a)
			}
		}
	}
	view, err := rc.CreateSchedule(cmd.Context(), client.ScheduleRequest{
		Name: n.Name, Cron: n.CronExpr, Kind: n.Kind, Argv: n.Args.Argv, Target: n.Args.Target,
		Source: n.Args.Source, TolerateUnreachable: n.Args.TolerateUnreachable,
		ConcurrencyKey: n.ConcurrencyKey, JitterS: n.JitterS,
	})
	if err != nil {
		return remoteAdminFail(cmd, "schedule add", err)
	}
	return remoteAdminOut(cmd, client.KindSchedule, view, func() {
		printScheduleAdded(cmd, view.Name, view.Kind, view.Cron)
	})
}

func runRemoteScheduleRm(cmd *cobra.Command, rc *client.Client, name string) error {
	res, err := rc.RemoveSchedule(cmd.Context(), name)
	if err != nil {
		return remoteAdminFail(cmd, "schedule rm", err)
	}
	return remoteAdminOut(cmd, client.KindScheduleRemoved, res, func() {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "schedule %s removed\n", res.Name)
	})
}

func runRemoteScheduleSetEnabled(cmd *cobra.Command, rc *client.Client, name string, enabled bool) error {
	view, err := rc.SetScheduleEnabled(cmd.Context(), name, enabled)
	if err != nil {
		return remoteAdminFail(cmd, "schedule "+enabledVerb(enabled), err)
	}
	return remoteAdminOut(cmd, client.KindSchedule, view, func() {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"schedule %s %s; the daemon picks it up on its next reload, or at once with `exocomp reload`\n",
			view.Name, enabledLabel(view.Enabled))
	})
}

// runRemoteRuleAdd sends a rule to the daemon. Its --ssh-identity paths name
// files on the daemon, which grants only those its operator allowed.
func runRemoteRuleAdd(cmd *cobra.Command, rc *client.Client, n store.NewRule, sshIdentity []string) error {
	if n.ActionKind != store.ActionPlaybook {
		sshIdentity = nil
	}
	for _, id := range sshIdentity {
		if !filepath.IsAbs(id) {
			return fmt.Errorf("rule add: --ssh-identity %q is not absolute, "+
				"and with a remote selected it names a path on the daemon", id)
		}
	}
	a := n.Action
	req := client.RuleRequest{
		Name: n.Name, SourceFilter: n.SourceFilter, Expr: n.CELExpr, Action: n.ActionKind,
		Argv: a.Argv, Workspace: a.Workspace, Source: a.Source, Playbook: a.Playbook, Limit: a.Limit,
		Tags: a.Tags, Check: a.Check, Diff: a.Diff, Egress: a.Egress, SSHIdentityFiles: sshIdentity,
		CooldownS: n.CooldownS, DailyCap: n.DailyCap, RequireApproval: n.RequireApproval,
	}
	if a.Timeout != 0 {
		req.Timeout = time.Duration(a.Timeout).String()
	}
	view, err := rc.CreateRule(cmd.Context(), req)
	if err != nil {
		return remoteAdminFail(cmd, "rule add", err)
	}
	return remoteAdminOut(cmd, client.KindRule, view, func() {
		printRuleAdded(cmd, view.Name, view.ActionKind, view.RequireApproval)
	})
}

func runRemoteRuleRm(cmd *cobra.Command, rc *client.Client, name string) error {
	res, err := rc.RemoveRule(cmd.Context(), name)
	if err != nil {
		return remoteAdminFail(cmd, "rule rm", err)
	}
	return remoteAdminOut(cmd, client.KindRuleRemoved, res, func() {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "rule %s removed\n", res.Name)
	})
}

func runRemoteRuleSetEnabled(cmd *cobra.Command, rc *client.Client, name string, enabled bool) error {
	view, err := rc.SetRuleEnabled(cmd.Context(), name, enabled)
	if err != nil {
		return remoteAdminFail(cmd, "rule "+enabledVerb(enabled), err)
	}
	return remoteAdminOut(cmd, client.KindRule, view, func() {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "rule %s %s\n", view.Name, enabledLabel(view.Enabled))
	})
}
