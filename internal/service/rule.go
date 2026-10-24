package service

import (
	"context"
	"strings"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// CreateRule checks a new rule against the engine that will run it and records
// it: the expression compiles in the engine's environment, the action's fields
// fit its kind, and a playbook action resolves to a plain playbook in its
// source's checkout with ansible-valid --limit and --tags. A rule that could
// never fire is refused here rather than logged on every event. The CLI and
// POST /api/v1/rules call it; a face that takes ssh identity paths from a
// caller on another host checks them against the daemon's allowlist first.
// The row records actor as its creator, as CreateSchedule does.
func (s *Service) CreateRule(ctx context.Context, n store.NewRule, actor Actor) (store.Rule, error) {
	a := n.Action
	switch n.ActionKind {
	case store.ActionShell:
		if len(a.Argv) == 0 {
			return store.Rule{}, invalid("action shell needs argv")
		}
	case store.ActionTofuPlan, store.ActionTofuApply:
		if a.Workspace == "" {
			return store.Rule{}, invalid("action %s needs a workspace", n.ActionKind)
		}
		if len(a.Argv) != 0 {
			return store.Rule{}, invalid("action %s takes no argv", n.ActionKind)
		}
	case store.ActionPlaybook:
		switch {
		case a.Source == "":
			return store.Rule{}, invalid("action playbook needs a source")
		case a.Playbook == "":
			return store.Rule{}, invalid("action playbook needs a playbook")
		case a.Limit == "":
			return store.Rule{}, invalid("action playbook needs a limit (default-deny)")
		case len(a.Argv) != 0:
			return store.Rule{}, invalid("action playbook takes no argv")
		}
		if err := s.checkPlaybookRule(ctx, a); err != nil {
			return store.Rule{}, err
		}
	case store.ActionNotify:
		if len(a.Argv) != 0 {
			return store.Rule{}, invalid("action notify takes no argv")
		}
		if n.RequireApproval {
			return store.Rule{}, invalid("action notify creates no job, so require-approval has nothing to gate")
		}
	default:
		return store.Rule{}, invalid("action must be one of %s", strings.Join(RuleActionKinds(), ", "))
	}
	if n.CooldownS < 0 {
		return store.Rule{}, invalid("cooldown cannot be negative")
	}
	if n.DailyCap < 0 {
		return store.Rule{}, invalid("daily cap cannot be negative")
	}
	if _, err := reactive.Compile(n.CELExpr); err != nil {
		return store.Rule{}, invalid("%v", err)
	}
	n.CreatedBy = ActorLabel(actor)
	return s.store.CreateRule(ctx, n)
}

// RuleActionKinds lists the action kinds a rule may take.
func RuleActionKinds() []string {
	return []string{store.ActionShell, store.ActionTofuPlan, store.ActionTofuApply,
		store.ActionPlaybook, store.ActionNotify}
}

// checkPlaybookRule refuses a playbook action that would never run. A limit
// carrying the {{host}} template is checked with a placeholder host, since the
// template resolves at request time.
func (s *Service) checkPlaybookRule(ctx context.Context, a store.RuleAction) error {
	if _, _, _, err := s.ResolvePlainSourcePlay(ctx, a.Source, a.Playbook); err != nil {
		return sourcePlayRefusal(err)
	}
	limit := strings.ReplaceAll(a.Limit, "{{host}}", "host")
	tags := strings.Join(a.Tags, ",")
	if _, _, err := AnsiblePassthrough(limit, tags, "", a.Check, a.Diff, nil); err != nil {
		return invalid("%v", err)
	}
	if _, err := store.NormalizeEgressPorts(a.Egress); err != nil {
		return invalid("%v", err)
	}
	if _, err := store.NormalizeSSHIdentityFiles(a.SSHIdentityFiles); err != nil {
		return invalid("%v", err)
	}
	return nil
}
