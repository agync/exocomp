package deps

import (
	"fmt"
	"strings"
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

// parsePyprojectDeps extracts the PEP 621 dependency specifiers from a
// pyproject.toml: the [project] table's `dependencies` array and every array
// under [dependency-groups]. It is a bounded reader for exactly those two
// shapes — arrays of quoted PEP 508 strings — not a general TOML parser: uv is
// what installs them, this only needs to know which strings to hand it.
//
// The limitation is deliberate and fail-safe: a dependency expressed in a form
// this does not recognise yields fewer specifiers, never a wrong one, and the
// manifest hash is still taken over the whole file so a change always
// re-resolves. A structurally broken array (an unterminated bracket) is an
// error so a malformed manifest fails loudly rather than silently under-installs.
func parsePyprojectDeps(body []byte) ([]string, error) {
	var (
		specs   []string
		section string
		// capturing accumulates an array value that spans lines until its
		// brackets balance; want records whether the captured key is one we
		// collect specifiers from.
		capturing bool
		want      bool
		depth     int
		buf       strings.Builder
	)

	flush := func() {
		if want {
			specs = append(specs, quotedStrings(buf.String())...)
		}
		buf.Reset()
		capturing = false
		want = false
		depth = 0
	}

	for _, raw := range strings.Split(string(body), "\n") {
		line := stripTOMLComment(raw)
		if !capturing {
			if h, ok := tableHeader(line); ok {
				section = h
				continue
			}
			key, value, ok := splitKeyValue(line)
			if !ok {
				continue
			}
			if !strings.HasPrefix(strings.TrimSpace(value), "[") {
				continue
			}
			want = isDepKey(section, key)
			capturing = true
			depth = 0
		}
		// Accumulate the (possibly partial) array text and track bracket depth
		// so a multi-line array closes on its own matching ']'.
		text := line
		if capturing {
			buf.WriteString(text)
			buf.WriteByte('\n')
			depth += strings.Count(stripQuoted(text), "[")
			depth -= strings.Count(stripQuoted(text), "]")
			if depth <= 0 {
				flush()
			}
		}
	}
	if capturing {
		return nil, fmt.Errorf("pyproject: unterminated array in [%s]", section)
	}
	return specs, nil
}

// isDepKey reports whether a key in the current section carries dependency
// specifiers: `dependencies` under [project], or any key under
// [dependency-groups] and its subtables.
func isDepKey(section, key string) bool {
	if section == "project" && key == "dependencies" {
		return true
	}
	return section == "dependency-groups" || strings.HasPrefix(section, "dependency-groups.")
}

// tableHeader returns the header name of a `[section]` line, without brackets.
func tableHeader(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' && s[1] != '[' {
		return strings.TrimSpace(s[1 : len(s)-1]), true
	}
	return "", false
}

// splitKeyValue splits a `key = value` line, trimming the key of surrounding
// quotes so a quoted group name in [dependency-groups] still matches.
func splitKeyValue(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	key = strings.Trim(key, `"'`)
	return key, line[i+1:], true
}

// quotedStrings returns every single- or double-quoted string in s. It is what
// turns a captured array's text into specifiers, and it ignores anything
// outside quotes (commas, brackets, whitespace).
func quotedStrings(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '"' && c != '\'' {
			continue
		}
		if j := strings.IndexByte(s[i+1:], c); j >= 0 {
			if inner := strings.TrimSpace(s[i+1 : i+1+j]); inner != "" {
				out = append(out, inner)
			}
			i += j + 1
		}
	}
	return out
}

// stripQuoted blanks the contents of quoted strings so bracket counting does
// not miscount a '[' or ']' that appears inside a version marker string.
func stripQuoted(s string) string {
	var b strings.Builder
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// stripTOMLComment drops a trailing comment, honouring quotes so a '#' inside a
// specifier string is not mistaken for a comment.
func stripTOMLComment(line string) string {
	inQuote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
		case c == '#':
			return line[:i]
		}
	}
	return line
}
