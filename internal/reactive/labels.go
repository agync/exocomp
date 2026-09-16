package reactive

import (
	"fmt"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// SourceLabel renders a rule's event-source filter the way `rule list` and the
// rules page read it: empty (every source) reads "any".
func SourceLabel(filter string) string {
	if filter == "" {
		return "any"
	}
	return filter
}

// ActionLabel renders a rule's action the way `rule list` prints it, so the
// CLI and the browser read a rule's action identically.
func ActionLabel(r store.Rule) string {
	switch r.ActionKind {
	case store.ActionShell:
		return r.ActionKind + " " + strings.Join(r.Action.Argv, " ")
	case store.ActionTofuPlan, store.ActionTofuApply:
		return r.ActionKind + " " + r.Action.Workspace
	case store.ActionPlaybook:
		if r.Action.LimitHosts {
			return fmt.Sprintf("%s %s %s (limit: event hosts)", r.ActionKind, r.Action.Source, r.Action.Playbook)
		}
		return fmt.Sprintf("%s %s %s (limit %s)", r.ActionKind, r.Action.Source, r.Action.Playbook, r.Action.Limit)
	default:
		return r.ActionKind
	}
}

// CooldownLabel renders a rule's cooldown; a zero cooldown reads "-".
func CooldownLabel(seconds int) string {
	if seconds <= 0 {
		return "-"
	}
	return (time.Duration(seconds) * time.Second).String()
}

// CapLabel renders a rule's daily cap; an unset cap reads "-".
func CapLabel(limit int) string {
	if limit <= 0 {
		return "-"
	}
	return fmt.Sprint(limit)
}

// ApprovalLabel says why a rule is gated, because "destructive" is a property
// of the action rather than a flag the operator set.
func ApprovalLabel(r store.Rule) string {
	switch {
	case Destructive(r.ActionKind):
		return "always (destructive)"
	case r.RequireApproval:
		return "required"
	default:
		return "-"
	}
}
