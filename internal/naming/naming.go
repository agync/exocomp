// Package naming validates the names exocomp turns into filesystem path
// components: workspace names, source names, and anything derived from them.
// It is the one place that decision is made, so a traversal-shaped name
// (finding #2) is refused once, at creation, instead of re-implemented per
// call site with the risk of the copies drifting apart.
package naming

import "fmt"

// maxLen bounds a component well under the usual filesystem NAME_MAX (255),
// leaving headroom for the suffixes callers append downstream (".tfplan",
// ".json", a "-<unixnano>" run id) without the joined path ever growing
// unbounded.
const maxLen = 200

// Component reports whether name is a single, safe path component: non-empty,
// at most maxLen bytes, never "." or "..", and drawn only from an
// allow-listed charset. Allow-listing rather than denying "/", "\", NUL, and
// friends is what makes "no separator, no NUL" hold for every byte instead of
// only the ones a deny-list happened to name.
func Component(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > maxLen {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// ValidateComponent returns a caller-facing error when name fails Component,
// naming what kind of name it was (e.g. "workspace", "source") so the message
// points at the argument that needs fixing.
func ValidateComponent(kind, name string) error {
	if Component(name) {
		return nil
	}
	return fmt.Errorf("%s name %q is not a safe name: use only letters, digits, "+
		"'-', '_', '.', 1-%d bytes, and not \".\" or \"..\"", kind, name, maxLen)
}
