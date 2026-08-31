package web

import (
	"net/http"

	"exocomp.dev/exocomp/internal/reactive"
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
// action and guardrails through the same reactive label helpers `rule list`
// uses, so the page and the CLI agree on what a rule does.
func (s *Server) rules(r *http.Request) ([]ruleRow, error) {
	list, err := s.store.ListRules(r.Context(), false)
	if err != nil {
		return nil, err
	}
	out := make([]ruleRow, 0, len(list))
	for _, rl := range list {
		out = append(out, ruleRow{
			Name: rl.Name, Enabled: rl.Enabled,
			SourceFilter: reactive.SourceLabel(rl.SourceFilter),
			Action:       reactive.ActionLabel(rl),
			Cooldown:     reactive.CooldownLabel(rl.CooldownS),
			Cap:          reactive.CapLabel(rl.DailyCap),
			Approval:     reactive.ApprovalLabel(rl),
			Expr:         rl.CELExpr,
		})
	}
	return out, nil
}
