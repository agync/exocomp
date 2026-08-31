package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// rulesVM is the /rules surface: the CEL expressions and actions that turn
// events into jobs, and the guardrails that bound them. Rules are
// CLI-managed (`rule add|enable|disable|rm`) — the browser reads them only,
// the pattern sources/secrets/runners set for CLI-managed resources.
type rulesVM struct {
	Page     page
	Operator bool
	Rules    []ruleRow
}

// ruleRow is one rule on the rules surface: its enabled posture, the source
// filter, the action as `rule list` renders it, and the guardrails. The
// expression is shown verbatim so an operator sees exactly what fires the job.
type ruleRow struct {
	Name         string
	Enabled      bool
	SourceFilter string
	Action       string
	Cooldown     string
	Cap          string
	Approval     string
	Expr         string
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.rules(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ident, _ := identityOf(r.Context())
	vm := rulesVM{
		Page: s.pageFor(r, "Rules", "rules"), Rules: rows,
		Operator: canWrite(ident),
	}
	s.render(w, r, http.StatusOK, rulesList(vm))
}

// rules reads every rule (enabled and disabled) into rows, rendering the
// action and guardrails exactly the way `rule list` prints them so the page
// and the CLI agree on what a rule does.
func (s *Server) rules(r *http.Request) ([]ruleRow, error) {
	list, err := s.store.ListRules(r.Context(), false)
	if err != nil {
		return nil, err
	}
	out := make([]ruleRow, 0, len(list))
	for _, rl := range list {
		out = append(out, ruleRow{
			Name: rl.Name, Enabled: rl.Enabled,
			SourceFilter: sourceLabel(rl.SourceFilter),
			Action:       actionLabel(rl),
			Cooldown:     cooldownLabel(rl.CooldownS),
			Cap:          capLabel(rl.DailyCap),
			Approval:     approvalLabel(rl),
			Expr:         rl.CELExpr,
		})
	}
	return out, nil
}

// The label helpers mirror cmd/exocomp/rule.go, which `rule list` uses. The
// web package cannot import the CLI's main package, so the exact strings are
// repeated here to keep the two surfaces reading identically.

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
	case store.ActionPlaybook:
		return fmt.Sprintf("%s %s %s (limit %s)", r.ActionKind, r.Action.Source, r.Action.Playbook, r.Action.Limit)
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
