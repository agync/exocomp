package policy_test

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/store"
)

// deleteBearingPlan is the activation for a plan destroying one resource and
// updating another — the values-free view a policy sees.
func deleteBearingPlan() map[string]any {
	return policy.Plan(true, []policy.PlanChange{
		{Address: "null_resource.keep", Actions: []string{"update"}},
		{Address: "null_resource.gone", Actions: []string{"delete"}},
	}, nil)
}

// createOnlyPlan destroys nothing: the mutation-check input that the delete
// rule keys on the action and not merely on "a plan with changes".
func createOnlyPlan() map[string]any {
	return policy.Plan(true, []policy.PlanChange{
		{Address: "null_resource.new", Actions: []string{"create"}},
	}, nil)
}

// noChangePlan is what a plan with nothing to do binds: an empty
// resource_changes list, which a policy walking it must read as clean, not error.
func noChangePlan() map[string]any {
	return policy.Plan(false, nil, nil)
}

const deleteExpr = `plan.resource_changes.exists(c, c.change.actions.exists(a, a == "delete"))`

func TestCompileRejectsInvalid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		expr string
	}{
		{"empty", ""},
		{"syntax", "plan.resource_changes.exists(c,"},
		{"unknown-var", `event.foo == "bar"`},
		{"not-bool", `plan.resource_changes`},
	} {
		if _, err := policy.Compile(tc.expr); err == nil {
			t.Errorf("Compile(%s %q): want an error, got none", tc.name, tc.expr)
		}
	}
}

func TestCompileAcceptsTheDeleteRule(t *testing.T) {
	t.Parallel()
	if _, err := policy.Compile(deleteExpr); err != nil {
		t.Fatalf("Compile(delete rule): %v", err)
	}
}

func TestMatchDetectsDelete(t *testing.T) {
	t.Parallel()
	prg, err := policy.Compile(deleteExpr)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	matched, err := prg.Match(context.Background(), deleteBearingPlan())
	if err != nil {
		t.Fatalf("Match(delete plan): %v", err)
	}
	if !matched {
		t.Fatal("the delete rule did not match a delete-bearing plan")
	}

	// Mutation check: a plan with changes but no delete must NOT match, or the
	// rule is really matching "any change" and the deny is indiscriminate.
	matched, err = prg.Match(context.Background(), createOnlyPlan())
	if err != nil {
		t.Fatalf("Match(create-only plan): %v", err)
	}
	if matched {
		t.Fatal("the delete rule matched a plan that deletes nothing")
	}
}

// TestMatchNoChangePlanIsClean proves the empty list is walkable: the delete
// rule returns a clean false against a no-change plan rather than erroring,
// which would fail the plan closed.
func TestMatchNoChangePlanIsClean(t *testing.T) {
	t.Parallel()
	prg, err := policy.Compile(deleteExpr)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	matched, err := prg.Match(context.Background(), noChangePlan())
	if err != nil {
		t.Fatalf("Match(no-change plan): %v", err)
	}
	if matched {
		t.Fatal("the delete rule matched a no-change plan")
	}
}

// TestMatchOnDrift proves resource_drift is bound too, so a policy can gate on
// what moved under tofu, not only on pending config changes.
func TestMatchOnDrift(t *testing.T) {
	t.Parallel()
	prg, err := policy.Compile(`plan.resource_drift.exists(c, c.change.actions.exists(a, a == "delete"))`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	plan := policy.Plan(true, nil, []policy.PlanChange{
		{Address: "null_resource.vanished", Actions: []string{"delete"}},
	})
	matched, err := prg.Match(context.Background(), plan)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !matched {
		t.Fatal("a drift-delete was not matched by a resource_drift rule")
	}
}

func TestEvaluateDenyOutranksApproval(t *testing.T) {
	t.Parallel()
	policies := []store.PlanPolicy{
		{Name: "a-approve", Expr: "true", Action: store.PolicyRequireApproval},
		{Name: "z-deny", Expr: deleteExpr, Action: store.PolicyDeny},
	}
	v, err := policy.Evaluate(context.Background(), policies, deleteBearingPlan())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.Deny() || v.Policy != "z-deny" {
		t.Fatalf("verdict = %+v, want deny by z-deny", v)
	}
}

func TestEvaluateRequireApprovalWhenNoDeny(t *testing.T) {
	t.Parallel()
	policies := []store.PlanPolicy{
		{Name: "no-delete", Expr: deleteExpr, Action: store.PolicyDeny},
		{Name: "touches-anything", Expr: "size(plan.resource_changes) > 0", Action: store.PolicyRequireApproval},
	}
	v, err := policy.Evaluate(context.Background(), policies, createOnlyPlan())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.RequireApproval() || v.Policy != "touches-anything" {
		t.Fatalf("verdict = %+v, want require_approval by touches-anything", v)
	}
}

func TestEvaluateCleanWhenNoneMatch(t *testing.T) {
	t.Parallel()
	policies := []store.PlanPolicy{
		{Name: "no-delete", Expr: deleteExpr, Action: store.PolicyDeny},
	}
	v, err := policy.Evaluate(context.Background(), policies, createOnlyPlan())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Matched() {
		t.Fatalf("verdict = %+v, want a clean (unmatched) verdict", v)
	}
}

// TestEvaluateFailsClosedOnMatchError proves a policy that cannot decide a plan
// blocks it: an expression that errors at evaluation is returned as an error,
// never skipped into a silent allow.
func TestEvaluateFailsClosedOnMatchError(t *testing.T) {
	t.Parallel()
	policies := []store.PlanPolicy{
		{Name: "broken", Expr: "plan.resource_changes[99].change.actions.size() > 0", Action: store.PolicyDeny},
	}
	_, err := policy.Evaluate(context.Background(), policies, createOnlyPlan())
	if err == nil {
		t.Fatal("Evaluate with an erroring policy: want an error, got none")
	}
}
