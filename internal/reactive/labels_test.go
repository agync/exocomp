package reactive_test

import (
	"testing"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// TestRuleLabelsMatchRuleList pins the shared label helpers both the CLI's
// `rule list` and the web rules page render through: the playbook action's
// one-line shape, the destructive approval note, and the empty-source "any".
// No harness involved — the labels are pure projections of a store.Rule.
func TestRuleLabelsMatchRuleList(t *testing.T) {
	if got := reactive.ActionLabel(store.Rule{
		ActionKind: store.ActionPlaybook,
		Action:     store.RuleAction{Source: "infra", Playbook: "site.yml", Limit: "laptops"},
	}); got != "playbook infra site.yml (limit laptops)" {
		t.Fatalf("ActionLabel(playbook) = %q", got)
	}

	if got := reactive.ApprovalLabel(store.Rule{ActionKind: store.ActionTofuApply}); got != "always (destructive)" {
		t.Fatalf("ApprovalLabel(destructive) = %q", got)
	}

	if got := reactive.SourceLabel(""); got != "any" {
		t.Fatalf("SourceLabel(empty) = %q", got)
	}
}
