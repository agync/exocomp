package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ansibleFlagValue is a shell-free argv token: no NUL or newline, which are the
// only bytes exec cannot carry in a single argument. A flag's value is one argv
// token after its flag, so it cannot introduce a second flag — validation here
// is against a value that would truncate or split the argument, not injection.
func ansibleFlagValue(flag, value string) error {
	if strings.ContainsAny(value, "\x00\n\r") {
		return fmt.Errorf("playbook run: %s value contains a newline or NUL", flag)
	}
	return nil
}

// extraVarKey is the ansible variable-name shape: a leading letter or
// underscore, then letters, digits, underscores. Rejecting anything else is
// what stops a crafted `-e` key from carrying flag-like or whitespace-split
// text into the run.
var extraVarKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// AnsiblePassthrough splits the operator's flags into the argv passthrough (the
// public flags ansible reads before the playbook) and the extra-vars map (kept
// off argv, written to a run-private file). A key=val is validated and mapped; a
// @file is resolved to an absolute path and passed through as `-e @path`, its
// values never leaving the file. Secrets never travel here: the vault password
// and run-secrets keep their own out-of-argv paths untouched. Every face that
// enqueues a play validates through this one seam.
func AnsiblePassthrough(limit, tags, skipTags string, check, diff bool, extraVars []string) ([]string, map[string]string, error) {
	var argv []string
	for flag, value := range map[string]string{"--limit": limit, "--tags": tags, "--skip-tags": skipTags} {
		if value == "" {
			continue
		}
		if err := ansibleFlagValue(flag, value); err != nil {
			return nil, nil, err
		}
	}
	// Rebuild in a stable order: a map iteration is unordered and the argv a
	// job records must be deterministic.
	for _, f := range []struct {
		flag, value string
	}{{"--limit", limit}, {"--tags", tags}, {"--skip-tags", skipTags}} {
		if f.value != "" {
			argv = append(argv, f.flag, f.value)
		}
	}
	if check {
		argv = append(argv, "--check")
	}
	if diff {
		argv = append(argv, "--diff")
	}

	var vars map[string]string
	for _, raw := range extraVars {
		if strings.HasPrefix(raw, "@") {
			path, err := filepath.Abs(raw[1:])
			if err != nil {
				return nil, nil, fmt.Errorf("playbook run: resolve -e %s: %w", raw, err)
			}
			if _, err := os.Stat(path); err != nil {
				return nil, nil, fmt.Errorf("playbook run: -e %s: %w", raw, err)
			}
			argv = append(argv, "-e", "@"+path)
			continue
		}
		key, val, ok := strings.Cut(raw, "=")
		if !ok {
			return nil, nil, fmt.Errorf("playbook run: -e %q is neither key=val nor @file", raw)
		}
		if !extraVarKey.MatchString(key) {
			return nil, nil, fmt.Errorf("playbook run: -e %q has an invalid variable name", key)
		}
		if err := ansibleFlagValue("-e", val); err != nil {
			return nil, nil, err
		}
		if vars == nil {
			vars = map[string]string{}
		}
		vars[key] = val
	}
	return argv, vars, nil
}
