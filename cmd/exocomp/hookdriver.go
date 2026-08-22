package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/verify"
)

// hookJobDriver runs a queued hook step: a sandboxed shell command with the
// checkout as its working directory. A hook's optional run_secret binding is
// deliberately absent from the job row — only its NAME is stored — so the driver
// resolves the sealed values from the envelope at execution time, exactly as an
// ansible or tofu job does, and injects them as named environment variables the
// child inherits. The values never touch the argv or the ledger.
type hookJobDriver struct {
	store   *store.Store
	env     *secrets.Envelope
	dataDir string
}

// Run implements executor.Driver.
func (d hookJobDriver) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	job, err := d.store.GetJob(ctx, spec.JobID)
	if err != nil {
		return executor.Result{}, err
	}

	runSecrets, secretPlain, err := resolveRunSecrets(ctx, d.store, d.env, job.Args.RunSecretBinding, "")
	if err != nil {
		return executor.Result{}, err
	}
	spec.Env, err = hookEnv(runSecrets)
	if err != nil {
		return executor.Result{}, err
	}

	drv := hookTierDriver(d.dataDir, spec.Dir)
	if len(secretPlain) == 0 {
		return drv.Run(ctx, spec, sink)
	}
	// The bound secret's plaintext is scrubbed from this hook's output on top of
	// the queue's sealed-secret redactor, the same belt-and-suspenders the
	// ansible and tofu job drivers apply to the material they resolve per run.
	return drv.Run(ctx, spec, runSink{redactor: secrets.NewRedactor(secretPlain), next: sink})
}

// hookTierDriver returns the shell driver a hook runs through for the effective
// tier: under the sandbox the command is wrapped in nono with the checkout
// granted read+write and the shell's own tool tree granted read — and the data
// dir's keys and database denied by the profile guard regardless of what the
// hook tries. A host that cannot sandbox runs the hook at the subprocess
// boundary, the same loud degradation every other job kind takes.
func hookTierDriver(dataDir, sourceRoot string) executor.Driver {
	if !sandboxed(context.Background()) {
		return executor.NewShell()
	}
	opts := sandboxOptions(dataDir)
	if sourceRoot != "" {
		opts.WritePaths = append(slices.Clone(opts.WritePaths), sourceRoot)
	}
	// The shell itself, plus whatever sits in its bin directory (the coreutils a
	// hook composes), is granted read; the system libraries it links against are
	// covered by nono's own system profile.
	opts.ReadPaths = append(opts.ReadPaths, executor.ToolGrants("sh")...)
	// A hook is local prep, not a network step (the gate is what probes the
	// network, in the daemon). The "hook" kind adds no egress port, so nono keeps
	// the network blocked.
	return executor.NewSandbox(hooks.JobKind, opts, nil)
}

// hookEnv renders a hook's resolved run secrets into the child's environment on
// top of the daemon's own, so a shell command keeps its PATH while gaining the
// bound variables. It refuses a value carrying a NUL, which would truncate the
// entry; the names were validated when the binding was defined.
func hookEnv(rs executor.RunSecrets) ([]string, error) {
	env := os.Environ()
	names := make([]string, 0, len(rs))
	for name := range rs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		v := rs[name]
		if strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("hook run secret %q value contains a NUL", name)
		}
		env = append(env, name+"="+v)
	}
	return env, nil
}

// verifyJobDriver runs a health/verification gate step: it decodes the declared
// probes from the job's argv and runs them in the daemon — a control-plane read
// over local or reachable endpoints, like drift collection — never a subprocess.
// The gate succeeds (exit 0) only when every probe passes; a single failure is a
// non-zero exit that fails the step, which blocks the rest of the workflow and
// marks the run failed even though the play before it exited 0. Probe output
// flows through the queue's redactor via the sink it is handed.
type verifyJobDriver struct {
	runner verify.Runner
}

// Run implements executor.Driver.
func (d verifyJobDriver) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	probes, err := verify.Decode(spec.Argv)
	if err != nil {
		// A gate that cannot be decoded cannot decide, and a gate that cannot
		// decide must never pass: fail the driver rather than the probe set.
		return executor.Result{}, err
	}

	emit := func(line string) {
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelInfo, Msg: line})
	}
	ok, failed := d.runner.RunAll(ctx, probes, emit)
	if ok {
		sink.Emit(executor.Event{
			Ts:    time.Now().UTC(),
			Level: executor.LevelSystem,
			Msg:   fmt.Sprintf("gate passed: %d probe(s)", len(probes)),
		})
		return executor.Result{ExitCode: 0}, nil
	}

	sink.Emit(executor.Event{
		Ts:    time.Now().UTC(),
		Level: executor.LevelError,
		Msg:   "gate FAILED: " + strings.Join(failed, ", "),
	})
	return executor.Result{ExitCode: 1}, nil
}
