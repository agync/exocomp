package deps

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// requirementLines returns the dependency specifiers in a requirements.txt
// body: non-blank, non-comment lines that are not pip options. Include
// directives (-r/-c) and flags are dropped rather than passed to uv as if they
// were packages — a requirements file that is all options declares no deps.
func requirementLines(body []byte) []string {
	var out []string
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// stripComment drops a trailing "# ..." comment. A '#' inside a quoted marker
// is not a comment start, but requirements.txt markers do not quote '#', so a
// bare split is correct here and stays simple.
func stripComment(line string) string {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		return line[:i]
	}
	return line
}

// parsePyprojectDeps extracts the dependency specifiers a source declares in a
// pyproject.toml: the PEP 621 `[project].dependencies` array and every PEP 735
// group under `[dependency-groups]`. It decodes the file with a real TOML
// parser (github.com/BurntSushi/toml) rather than scanning lines, so every
// legal spelling of those tables is read correctly and a malformed file fails
// loudly instead of silently yielding fewer specifiers.
//
// What it deliberately does NOT collect: `[project].optional-dependencies`
// (extras are opt-in, not installed by default) and a `[dependency-groups]`
// entry's `include-group` reference (that names another group, not a package).
// uv is what installs the returned specifiers; this only decides which strings
// to hand it.
func parsePyprojectDeps(body []byte) ([]string, error) {
	var doc map[string]any
	if _, err := toml.Decode(string(body), &doc); err != nil {
		return nil, fmt.Errorf("pyproject: %w", err)
	}

	var specs []string
	if project, ok := doc["project"].(map[string]any); ok {
		specs = append(specs, stringItems(project["dependencies"])...)
	}
	if groups, ok := doc["dependency-groups"].(map[string]any); ok {
		// A TOML table has no inherent order; the group names are sorted so the
		// specifier list is stable across runs (the manifest hash and the uv
		// install line must not depend on map iteration order).
		for _, name := range sortedKeys(groups) {
			specs = append(specs, stringItems(groups[name])...)
		}
	}
	if err := validateSpecifiers(specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// validateSpecifiers refuses a dependency specifier that would reach uv as a
// flag rather than a package. A PEP 508 requirement names a
// package first — it never begins with `-`, so a leading dash means a
// `--index-url=…` redirect or a `-e /etc` editable smuggled through
// `[project.dependencies]`, where pip options have no business. Rejecting it
// fails the vendoring step loudly instead of handing uv a flag it did not mean
// to grant; the argv path also passes these past `--` as defence in depth.
func validateSpecifiers(specs []string) error {
	for _, s := range specs {
		if strings.HasPrefix(strings.TrimSpace(s), "-") {
			return fmt.Errorf("dependency specifier %q begins with '-'; a package requirement "+
				"cannot start a flag — refusing to pass it to the resolver", s)
		}
	}
	return nil
}

// stringItems returns the string elements of a decoded TOML array, skipping any
// non-string element. In `[dependency-groups]` a non-string element is a
// `{include-group = "..."}` table, which references another group rather than a
// package, so dropping it is correct — never collected as a bogus specifier.
func stringItems(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// sortedKeys returns a map's keys in lexical order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
