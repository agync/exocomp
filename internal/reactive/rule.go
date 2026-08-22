package reactive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/cel-go/cel"

	"exocomp.dev/exocomp/internal/store"
)

// evalBudget bounds one expression's evaluation. CEL is not Turing-complete,
// but its macros are still loops over whatever a sender put in a label set:
// arbitrary input must not be able to hold the ingress open.
const evalBudget = 50 * time.Millisecond

// costLimit bounds an expression's abstract cost, which stops a comprehension
// over a large label set before the wall clock has to.
const costLimit = 1_000_000

// celEnv is the whole vocabulary a rule may speak: one variable, the standard
// operators, and nothing that touches the host. Built once — every rule
// compiles against the same environment, so what `rule add` accepts is exactly
// what the engine will run.
var celEnv = sync.OnceValues(func() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)),
	)
})

// Program is one compiled rule expression.
type Program struct {
	expr string
	prg  cel.Program
}

// Compile checks an expression and returns the program the engine evaluates.
// It is called at `rule add` so a rule that cannot compile is refused at write
// time, and again when the engine loads rules, so a row that stopped
// compiling is reported rather than silently skipped.
func Compile(expr string) (*Program, error) {
	if expr == "" {
		return nil, errors.New("reactive: empty rule expression")
	}
	env, err := celEnv()
	if err != nil {
		return nil, fmt.Errorf("reactive: cel environment: %w", err)
	}

	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("reactive: compile %q: %w", expr, issues.Err())
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return nil, fmt.Errorf("reactive: compile %q: expression yields %s, want bool",
			expr, ast.OutputType())
	}

	prg, err := env.Program(ast,
		cel.EvalOptions(cel.OptOptimize),
		cel.CostLimit(costLimit),
		cel.InterruptCheckFrequency(100),
	)
	if err != nil {
		return nil, fmt.Errorf("reactive: program %q: %w", expr, err)
	}
	return &Program{expr: expr, prg: prg}, nil
}

// Match evaluates the program against an event under a hard time budget.
// An expression that errors does not match: a rule that cannot decide must not
// be able to fire an apply.
func (p *Program) Match(ctx context.Context, e Event) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, evalBudget)
	defer cancel()

	out, _, err := p.prg.ContextEval(ctx, map[string]any{"event": e.Activation()})
	if err != nil {
		return false, fmt.Errorf("reactive: evaluate %q: %w", p.expr, err)
	}
	matched, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("reactive: evaluate %q: result is %T, want bool", p.expr, out.Value())
	}
	return matched, nil
}

// Ruleset holds the enabled rules with their compiled expressions, in the
// order they are evaluated: by name, so which rule wins a tie is a fact the
// operator can read off `rule list` rather than a database artefact.
type Ruleset struct {
	rules    []store.Rule
	programs map[int64]*Program
	// broken records the rules that would not compile, by name. They are kept
	// out of evaluation and reported, never silently skipped.
	broken map[string]error
}

// LoadRules compiles every enabled rule. A rule that will not compile is left
// out of the set and recorded in Broken; the rest still run, because one bad
// row must not cost the operator every other rule.
func LoadRules(ctx context.Context, st *store.Store) (*Ruleset, error) {
	rows, err := st.ListRules(ctx, true)
	if err != nil {
		return nil, err
	}

	set := &Ruleset{
		programs: make(map[int64]*Program, len(rows)),
		broken:   map[string]error{},
	}
	for _, r := range rows {
		prg, err := Compile(r.CELExpr)
		if err != nil {
			set.broken[r.Name] = err
			continue
		}
		set.rules = append(set.rules, r)
		set.programs[r.ID] = prg
	}
	return set, nil
}

// Rules returns the compiled, enabled rules in evaluation order.
func (s *Ruleset) Rules() []store.Rule { return s.rules }

// Broken returns the rules that would not compile, by name.
func (s *Ruleset) Broken() map[string]error { return s.broken }

// Program returns a rule's compiled expression.
func (s *Ruleset) Program(ruleID int64) *Program { return s.programs[ruleID] }

// Applies reports whether a rule's source filter admits an event. An empty
// filter admits every source.
func Applies(r store.Rule, e Event) bool {
	return r.SourceFilter == "" || r.SourceFilter == e.Source
}
