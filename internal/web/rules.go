package web

import (
	"context"
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

// ruleRow is one rule on the rules surface, a two-line row (SPEC §5.3): line
// one the CEL expression (one line, ellipsis — never wraps), line two `then` +
// the action phrase + the guard words. LAST FIRED comes from the rule_fire
// ledger, not the rule row.
type ruleRow struct {
	Name    string
	Enabled bool
	// Action is line two's action phrase: "playbook {source}/{playbook}", a
	// shell argv, "log only · no action, no guards" for a notify rule, or a
	// tofu workspace verb.
	Action string
	// Guards are the 20px surface-control words (SPEC §5.3): egress {ports}
	// and cooldown {d} in surface-control ink, and Gate — "gate approval · TTL
	// {d}" — in wait-tint (the one guard that stops the run). Empty = absent.
	Egress   string
	Cooldown string
	Gate     string
	// Expr is the CEL expression, verbatim.
	Expr string
	// LastFire/FireFact are the ledger's newest fire: the clock and the outcome
	// ("job #N · exit {code}" / "matched · no job"). LastFire "—" and an empty
	// FireFact when the rule has never fired.
	LastFire string
	FireFact string
}

// HasGuards reports whether line two carries any guard word, so the template
// can draw the separator before the guard cluster.
func (r ruleRow) HasGuards() bool {
	return r.Egress != "" || r.Cooldown != "" || r.Gate != ""
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

// rules reads every rule (enabled and disabled) into two-line rows, rendering
// the action phrase and the guard words the same way the run path reads them,
// and the newest fire from the rule_fire ledger.
func (s *Server) rules(r *http.Request) ([]ruleRow, error) {
	list, err := s.store.ListRules(r.Context(), false)
	if err != nil {
		return nil, err
	}
	out := make([]ruleRow, 0, len(list))
	for _, rl := range list {
		row := ruleRow{
			Name:     rl.Name,
			Enabled:  rl.Enabled,
			Action:   ruleActionPhrase(rl),
			Cooldown: ruleCooldown(rl),
			Gate:     ruleGate(rl),
			Expr:     rl.CELExpr,
			LastFire: "—",
		}
		if rl.ActionKind == store.ActionPlaybook && len(rl.Action.Egress) > 0 {
			row.Egress = "egress " + joinPorts(rl.Action.Egress, ", ")
		}
		if fire, ferr := s.store.LastRuleFire(r.Context(), rl.ID); ferr == nil {
			row.LastFire = fire.Ts.Local().Format("15:04:05")
			if fire.JobID != nil {
				row.FireFact = s.fireFact(r.Context(), *fire.JobID)
			} else {
				row.FireFact = "matched · no job"
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// ruleActionPhrase renders line two's action the way the design reads it
// (SPEC §5.3): a playbook action as "playbook {source}/{playbook}", a shell
// argv joined, a notify rule as "log only · no action, no guards".
func ruleActionPhrase(rl store.Rule) string {
	switch rl.ActionKind {
	case store.ActionPlaybook:
		phrase := "playbook " + rl.Action.Source + "/" + rl.Action.Playbook
		if rl.Action.LimitHosts {
			phrase += " · limit from event hosts"
		}
		return phrase
	case store.ActionShell:
		return "shell " + strings.Join(rl.Action.Argv, " ")
	case store.ActionNotify:
		return "log only · no action, no guards"
	case store.ActionTofuPlan, store.ActionTofuApply:
		return rl.ActionKind + " " + rl.Action.Workspace
	default:
		return rl.ActionKind
	}
}

// ruleCooldown renders the cooldown guard word, or "" when the rule has none.
func ruleCooldown(rl store.Rule) string {
	if rl.CooldownS <= 0 {
		return ""
	}
	return "cooldown " + humanAge(time.Duration(rl.CooldownS)*time.Second)
}

// ruleGate renders the wait-tint gate guard, or "" when the rule never stops
// for approval. Destructive actions are gated whatever the rule says, the same
// default-deny the queue holds.
func ruleGate(rl store.Rule) string {
	if !rl.RequireApproval && !reactive.Destructive(rl.ActionKind) {
		return ""
	}
	return "gate approval · TTL " + humanAge(reactive.DefaultApprovalTTL)
}

// fireFact names the fire's job and its outcome: "job #N · exit {code}" when
// the job settled, "job #N · {state}" while it runs, and the bare id if the
// job is gone.
func (s *Server) fireFact(ctx context.Context, id int64) string {
	job, err := s.store.GetJob(ctx, id)
	if err != nil {
		return "job #" + itoa64(id)
	}
	if job.ExitCode != nil {
		return "job #" + itoa64(id) + " · exit " + itoa(int64(*job.ExitCode))
	}
	return "job #" + itoa64(id) + " · " + string(job.State)
}
