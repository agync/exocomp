package policy

import (
	"context"
	"fmt"

	"exocomp.dev/exocomp/internal/store"
)

// PlanChange is one resource's values-free move: its address, the actions tofu
// would take (create, update, delete, replace, …), and the attribute KEYS
// involved. Values never appear — a plan attribute is routinely a secret or a
// generated password — so a policy decides on structure alone, which is exactly
// what keeps the verdict and its inputs safe to persist (the governing rule:
// never expose raw state).
type PlanChange struct {
	Address  string
	Actions  []string
	AttrKeys []string
}

// Plan builds the activation a policy binds to `plan`. resource_changes are the
// pending config changes, resource_drift is what moved under tofu; each element
// is {address, change: {actions: [...]}, attr_keys: [...]}. changes is the bare
// "did anything move" bool. The shape mirrors the keys of `tofu show -json` a
// policy reaches for, minus the values.
func Plan(changes bool, resourceChanges, resourceDrift []PlanChange) map[string]any {
	return map[string]any{
		"changes":          changes,
		"resource_changes": changesToActivation(resourceChanges),
		"resource_drift":   changesToActivation(resourceDrift),
	}
}

func changesToActivation(cs []PlanChange) []any {
	out := make([]any, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]any{
			"address":   c.Address,
			"change":    map[string]any{"actions": anyStrings(c.Actions)},
			"attr_keys": anyStrings(c.AttrKeys),
		})
	}
	return out
}

// anyStrings copies into a []any so the CEL type adapter sees a homogeneous list
// of strings whatever the source slice's capacity games.
func anyStrings(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// Verdict is what evaluating a workspace's policies against a plan decided. A
// zero Verdict (empty Action) is the clean case: no policy matched, the plan is
// consumable. Otherwise Policy names the row that decided and Action is its
// store action (deny or require_approval).
type Verdict struct {
	Policy string
	Action string
}

// Matched reports whether any policy decided this plan.
func (v Verdict) Matched() bool { return v.Action != "" }

// Deny reports whether the verdict makes the plan unconsumable.
func (v Verdict) Deny() bool { return v.Action == store.PolicyDeny }

// RequireApproval reports whether the verdict routes the apply through the gate.
func (v Verdict) RequireApproval() bool { return v.Action == store.PolicyRequireApproval }

// Evaluate runs a workspace's policies against a plan activation and returns the
// governing verdict. deny outranks require_approval — a plan that any deny
// matches is unconsumable regardless of what else matched — and within a rank
// the first policy by name wins, so which policy is named is a fact the
// operator reads off `policy list` rather than a database artefact (the caller
// passes rows already ordered by name).
//
// A policy whose expression will not compile or errors while evaluating is not
// skipped: it is returned as an error, so the seam fails the seal closed. A
// policy that cannot decide must never let a plan through.
func Evaluate(ctx context.Context, policies []store.PlanPolicy, plan map[string]any) (Verdict, error) {
	var approval *Verdict
	for _, p := range policies {
		prg, err := Compile(p.Expr)
		if err != nil {
			return Verdict{}, fmt.Errorf("policy %q: %w", p.Name, err)
		}
		matched, err := prg.Match(ctx, plan)
		if err != nil {
			return Verdict{}, fmt.Errorf("policy %q: %w", p.Name, err)
		}
		if !matched {
			continue
		}
		if p.Action == store.PolicyDeny {
			return Verdict{Policy: p.Name, Action: store.PolicyDeny}, nil
		}
		if approval == nil {
			approval = &Verdict{Policy: p.Name, Action: store.PolicyRequireApproval}
		}
	}
	if approval != nil {
		return *approval, nil
	}
	return Verdict{}, nil
}
