// Package docexpr validates the CEL examples quoted in the operator docs
// against the same environments `rule add` and `policy add` compile against.
//
// A guide that quotes an expression which no longer compiles, names a field a
// normalized event does not carry, or compares the status to a value the
// pipeline never emits is a doc that will be copied and fail. This turns that
// drift into a failing test rather than a support question: TestDocsExamples
// runs Check over docs/, and `make check` runs it on every commit, so the guard
// is the existing gate rather than a new hook.
package docexpr

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/reactive"
)

// Finding is one `--expr` example that does not match the code it documents.
type Finding struct {
	File    string // path relative to the checked root
	Line    int    // 1-based line number
	Expr    string // the expression text as written
	Problem string // what is wrong with it
}

// String renders a finding in the compiler-diagnostic shape, so a failing test
// reads like a build error.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s (in %s)", f.File, f.Line, f.Problem, f.Expr)
}

// The expressions a doc quotes are the only text Check reads: a filename like
// `event.go` or a column like `event.check_observations_json` in prose must not
// be mistaken for an expression.
var (
	// exprValue matches the opening quote of an --expr value: `--expr "` or
	// `--expr '`. The value is read to the matching close quote on the same line.
	exprValue = regexp.MustCompile(`--expr\s+["']`)
	// refEvent and refPlan decide which environment an expression belongs to.
	// Member access, not the bare word, so a string literal naming "plan" or a
	// placeholder `...` classifies as neither and is skipped.
	refEvent = regexp.MustCompile(`\bevent\.`)
	refPlan  = regexp.MustCompile(`\bplan\.`)
	// eventMember captures a first-level `event.<field>` access.
	eventMember = regexp.MustCompile(`\bevent\.([A-Za-z_][A-Za-z0-9_]*)`)
	// statusLiteral captures `event.status == '<value>'` (or !=).
	statusLiteral = regexp.MustCompile(`\bevent\.status\s*(?:==|!=)\s*["']([A-Za-z0-9_]+)["']`)
)

// statuses are the only values a normalized event's status can take
// (internal/reactive/event.go): every source's vocabulary collapses onto these
// two, so a doc comparing event.status to anything else is wrong.
var statuses = map[string]bool{reactive.StatusFiring: true, reactive.StatusResolved: true}

// Check walks root for markdown files and validates every `--expr` value it
// finds. A finding is a doc example that would not work as written; a nil slice
// means every example matches the code.
func Check(root string) ([]Finding, error) {
	allowed := eventFields()
	var findings []Finding

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			expr, ok := extractExpr(line)
			if !ok {
				continue
			}
			findings = append(findings, validate(filepath.ToSlash(rel), i+1, expr, allowed)...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// extractExpr reads the value of an --expr flag on one line. It requires a
// quoted value, so the flag's own help spelling (`--expr event`) is not read as
// an expression, and it reads only the same line, which is how every example in
// the bundle is written.
func extractExpr(line string) (string, bool) {
	loc := exprValue.FindStringIndex(line)
	if loc == nil {
		return "", false
	}
	quote := line[loc[1]-1]
	rest := line[loc[1]:]
	end := strings.IndexByte(rest, quote)
	if end < 0 {
		return "", false // unterminated on this line; not an example to judge
	}
	return rest[:end], true
}

// validate compiles one expression against the environment its variable names
// and, for an event expression, checks every field it reaches for against the
// fields a normalized event actually carries.
func validate(file string, line int, expr string, allowed map[string]bool) []Finding {
	hasEvent := refEvent.MatchString(expr)
	hasPlan := refPlan.MatchString(expr)

	switch {
	case hasEvent && hasPlan:
		return []Finding{{file, line, expr, "expression references both event and plan"}}

	case hasPlan:
		if _, err := policy.Compile(expr); err != nil {
			return []Finding{{file, line, expr, firstLine(err.Error())}}
		}
		return nil

	case hasEvent:
		if _, err := reactive.Compile(expr); err != nil {
			return []Finding{{file, line, expr, firstLine(err.Error())}}
		}
		var out []Finding
		for _, m := range eventMember.FindAllStringSubmatch(expr, -1) {
			if !allowed[m[1]] {
				out = append(out, Finding{file, line, expr,
					fmt.Sprintf("event.%s is not a field of a normalized event", m[1])})
			}
		}
		for _, m := range statusLiteral.FindAllStringSubmatch(expr, -1) {
			if !statuses[m[1]] {
				out = append(out, Finding{file, line, expr,
					fmt.Sprintf("event.status compared to %q; the pipeline emits only firing or resolved", m[1])})
			}
		}
		return out

	default:
		// Neither variable: a placeholder like '...', not an expression to judge.
		return nil
	}
}

// eventFields is the set of first-level fields a rule may reach on `event`,
// read from the same Activation() the engine binds, so this list cannot drift
// from the event it documents.
func eventFields() map[string]bool {
	out := make(map[string]bool)
	for k := range (reactive.Event{}).Activation() {
		out[k] = true
	}
	return out
}

// firstLine trims a multi-line compiler error to the line a reader needs.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
