// Package policy is the CEL gate over an OpenTofu plan. A policy is one boolean
// expression over the `tofu show -json` document, bound to the variable `plan`;
// it is compiled and refused at write time so a rule that cannot decide is
// never stored, and evaluated when a plan is sealed so a delete-bearing or
// otherwise dangerous plan is caught before an apply can consume it.
//
// It reuses the reactive layer's compile-time discipline (a single variable, a
// bool output type, a cost and time budget), differing only in the variable it
// binds — `plan`, not `event` — and in its fail-closed evaluation: where a
// reactive rule that errors simply does not fire, a plan policy that cannot
// decide blocks, because silently allowing a plan a policy could not evaluate
// is the failure the gate exists to prevent.
package policy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
)

// evalBudget bounds one expression's evaluation. CEL is not Turing-complete,
// but its macros loop over whatever the plan document holds, and a large plan
// must not be able to hold the seal open.
const evalBudget = 50 * time.Millisecond

// costLimit bounds an expression's abstract cost, stopping a comprehension over
// a large resource_changes list before the wall clock has to.
const costLimit = 1_000_000

// celEnv is the whole vocabulary a policy may speak: the plan document, the
// standard operators, and nothing that touches the host. Built once — every
// policy compiles against the same environment, so what `policy add` accepts is
// exactly what the seal-time engine will run.
var celEnv = sync.OnceValues(func() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable(PlanVar, cel.MapType(cel.StringType, cel.DynType)),
	)
})

// PlanVar is the single variable a policy expression binds: the parsed
// `tofu show -json` document (resource_changes[], resource_drift[], …).
const PlanVar = "plan"

// Program is one compiled policy expression.
type Program struct {
	expr string
	prg  cel.Program
}

// Compile checks an expression and returns the program the engine evaluates.
// It is called at `policy add` so a policy that cannot compile is refused at
// write time, and again when the engine loads policies, so a row that stopped
// compiling is reported rather than silently skipped.
func Compile(expr string) (*Program, error) {
	if expr == "" {
		return nil, errors.New("policy: empty policy expression")
	}
	env, err := celEnv()
	if err != nil {
		return nil, fmt.Errorf("policy: cel environment: %w", err)
	}

	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("policy: compile %q: %w", expr, issues.Err())
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return nil, fmt.Errorf("policy: compile %q: expression yields %s, want bool",
			expr, ast.OutputType())
	}

	prg, err := env.Program(ast,
		cel.EvalOptions(cel.OptOptimize),
		cel.CostLimit(costLimit),
		cel.InterruptCheckFrequency(100),
	)
	if err != nil {
		return nil, fmt.Errorf("policy: program %q: %w", expr, err)
	}
	return &Program{expr: expr, prg: prg}, nil
}

// Match evaluates the program against a plan document under a hard time budget.
// Unlike a reactive rule, an expression that errors is a match failure the
// caller must surface, not a quiet false: a policy that cannot decide against a
// plan must block that plan, never wave it through.
func (p *Program) Match(ctx context.Context, plan map[string]any) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, evalBudget)
	defer cancel()

	out, _, err := p.prg.ContextEval(ctx, map[string]any{PlanVar: plan})
	if err != nil {
		return false, fmt.Errorf("policy: evaluate %q: %w", p.expr, err)
	}
	matched, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("policy: evaluate %q: result is %T, want bool", p.expr, out.Value())
	}
	return matched, nil
}
