package executor

import (
	"fmt"
	"slices"
	"strings"
)

// RunSecrets is the credential-plane binding: a sealed-secret set resolved
// to named environment variables for one run. It is what lets a
// dynamic-inventory plugin or a provider authenticate under the sandbox —
// the token reaches the process as an env var the allowlist would otherwise
// strip, because the driver injects it explicitly for that run rather than
// inheriting it from the daemon's own environment.
//
// The map is env name -> value. Values are the plaintext of sealed secrets,
// resolved by the caller from the envelope at execution time; the driver never
// reads the store. Keys map to the provider/plugin/TF_VAR_ names the tool
// reads.
type RunSecrets map[string]string

// runSecretEnv renders a RunSecrets map into sorted KEY=VALUE entries the child
// process inherits. It validates every name so a binding cannot smuggle a
// second variable through a "=" or a NUL, and refuses a value carrying a NUL,
// which would truncate the environment entry. Order is deterministic so a run's
// environment does not depend on map iteration.
func runSecretEnv(rs RunSecrets) ([]string, error) {
	if len(rs) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(rs))
	for name := range rs {
		if !isEnvName(name) {
			return nil, fmt.Errorf("executor: run secret env name %q is not a valid identifier", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)

	out := make([]string, 0, len(names))
	for _, name := range names {
		v := rs[name]
		if strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("executor: run secret %q value contains a NUL", name)
		}
		out = append(out, name+"="+v)
	}
	return out, nil
}

// isEnvName reports whether s is a POSIX-shaped environment variable name:
// a letter or underscore, then letters, digits, or underscores. A provider
// env name (PROXMOX_PASSWORD, TF_VAR_region, AWS_ACCESS_KEY_ID) is exactly
// this shape, and anything else is a mistake worth refusing loudly.
func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
