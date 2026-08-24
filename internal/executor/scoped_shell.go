package executor

import (
	"context"
	"os"
)

// scopedEnvAllow is what a scoped shell hands an operator-configured command
// when the caller set no explicit Env: enough for a command to resolve other
// binaries by name, nothing the daemon's own process happens to carry.
var scopedEnvAllow = []string{"PATH"}

// NewScopedShell returns a Driver like Shell, except a nil spec.Env — Go's
// exec.Cmd default meaning "inherit the caller's full environment" — is
// replaced with an explicit, minimal one instead of the daemon's own
// os.Environ(). It is what the queue's "rest" routing runs a reactive rule's
// or a schedule's shell action through: unlike the ansible/tofu/cli drivers,
// a bare shell action has no per-run env of its own to build, so without this
// it would otherwise inherit whatever the daemon process happens to export.
//
// A caller that already set spec.Env (a governed driver delegating to an
// inner Shell with its own curated env) is passed through unchanged.
func NewScopedShell() Driver { return scopedShell{inner: NewShell()} }

type scopedShell struct{ inner Driver }

// Run implements Driver.
func (d scopedShell) Run(ctx context.Context, spec Spec, sink EventSink) (Result, error) {
	if spec.Env == nil {
		spec.Env = scopedEnv()
	}
	return d.inner.Run(ctx, spec, sink)
}

func scopedEnv() []string {
	env := make([]string, 0, len(scopedEnvAllow))
	for _, name := range scopedEnvAllow {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}
