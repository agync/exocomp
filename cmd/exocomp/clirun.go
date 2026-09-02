package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newRunCLICmd)
}

// cliRunbookPrefix labels a governed CLI job so the job list tells one tool's
// runs from another's without reading argv.
const cliRunbookPrefix = "cli:"

// cliRunSpec is one governed CLI invocation.
type cliRunSpec struct {
	tool            string
	args            []string
	sourceName      string
	runSecrets      string
	runnerName      string
	concurrencyKey  string
	requireApproval bool
	// privileged runs the job unsandboxed (the disabled tier), full host access.
	privileged     bool
	timeout        time.Duration
	idempotencyKey string
	// json switches the report to the wire envelope and keeps the run's event
	// stream off stdout, so the envelope is the only thing printed there.
	json bool
}

func newRunCLICmd() *cobra.Command {
	var (
		sourceName      string
		runSecrets      string
		runnerName      string
		concurrencyKey  string
		requireApproval bool
		privileged      bool
		timeout         time.Duration
	)

	cmd := &cobra.Command{
		Use:   "run-cli <tool> [-- <args...>]",
		Short: "Run an allow-listed CLI through the queue, sandbox, and approval gate",
		Long: "The governed run type for work that is neither ansible nor tofu: a kubectl, " +
			"a scoped token refresh, a vendor deploy CLI. Today those escape governance " +
			"through an `ansible shell` wrapper; this runs them through the same machinery " +
			"a playbook or a tofu apply gets — the one queue, the nono sandbox, per-job " +
			"private directories, run-secret injection, redaction, and the approval gate.\n\n" +
			"Only a binary registered with `exocomp cli-tool add` can run. An unregistered " +
			"name is refused before any job row exists — the allow-list, not the caller, " +
			"decides what may execute.\n\n" +
			"Everything after `--` is the tool's own argv. It stays a list: there is no " +
			"shell anywhere on the path, so a semicolon, a pipe, a glob or a `$(...)` in an " +
			"argument reaches the process as one literal argument and means nothing. " +
			"--run-secrets binds a sealed set as named environment for the run, so a " +
			"provider token never appears on argv or in an event " +
			"(docs/operations/cli-run.md).",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("run-cli: name the registered tool to run")
			}
			dash := cmd.ArgsLenAtDash()
			if dash > 1 {
				return fmt.Errorf("run-cli: unexpected argument %q; the tool's arguments go after --", args[1])
			}
			if dash < 0 && len(args) > 1 {
				return fmt.Errorf("run-cli: put the tool's arguments after --, e.g. run-cli %s -- %s",
					args[0], args[1])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			var toolArgs []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				toolArgs = args[dash:]
			}
			return cliRunExit(cmd, cliRunSpec{
				tool:            args[0],
				args:            toolArgs,
				sourceName:      sourceName,
				runSecrets:      runSecrets,
				runnerName:      runnerName,
				concurrencyKey:  concurrencyKey,
				requireApproval: requireApproval,
				privileged:      privileged,
				timeout:         timeout,
				json:            mode == outputJSON,
			})
		},
	}
	cmd.Flags().StringVarP(&sourceName, "source", "s", "",
		"run from this registered source's checkout; it is the only repository path the run may reach")
	cmd.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as named env for the run (the provider token the tool authenticates with)")
	cmd.Flags().StringVar(&runnerName, "runner", "",
		"enforce this registered runner's memory_max/cpu_quota as a per-job cgroup cap "+
			"(local-sandboxed only; refused, never run uncapped, if it cannot be enforced)")
	cmd.Flags().StringVar(&concurrencyKey, "concurrency-key", "",
		"serialize this run against others holding the key (default: cli:<tool>; pass tofu:<workspace> to serialize with that workspace's runs)")
	cmd.Flags().BoolVar(&requireApproval, "require-approval", false,
		"gate the run behind the approval gate; it waits in awaiting_approval until a decision")
	cmd.Flags().BoolVar(&privileged, "privileged", false,
		"run this job UNSANDBOXED — full host access, no kernel sandbox; operator only (see docs/security/threat-model.md)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "kill the run after this long (0: no limit)")
	return cmd
}

// cliRunExit runs the spec and translates the job outcome into the process
// exit code, the same way `playbook run` and the ad-hoc verbs do.
func cliRunExit(cmd *cobra.Command, spec cliRunSpec) error {
	spec.idempotencyKey = idempotencyKey(cmd)
	code, err := cliRun(cmd, spec)
	if err != nil {
		if spec.json {
			return failJSON(cmd, err)
		}
		return err
	}
	if code != 0 {
		// A cobra RunE cannot carry an exit code; cliRun has already released
		// the store and the envelope by here.
		os.Exit(code)
	}
	return nil
}

// cliRun resolves the allow-list entry, refuses anything not on it, and runs
// the job. It returns the job's exit code, and an error only when the job could
// not be run at all.
func cliRun(cmd *cobra.Command, spec cliRunSpec) (code int, err error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out, warn := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if spec.privileged {
		warnPrivileged(warn)
	}
	// On the JSON path the envelope is the only thing on stdout, so the run's
	// streamed events go nowhere rather than interleaving with it.
	events := out
	if spec.json {
		events = io.Discard
	}

	dir, err := dataDir()
	if err != nil {
		return 0, fmt.Errorf("run-cli: resolve data dir: %w", err)
	}

	st, err := openJobStore(ctx, warn)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	tool, err := resolveCLITool(ctx, st, spec.tool)
	if err != nil {
		return 0, err
	}
	argv, err := cliRunArgv(tool, spec.args)
	if err != nil {
		return 0, err
	}

	root, rootReadOnly, err := sourceCheckoutRoot(ctx, st, dir, spec.sourceName, "run-cli")
	if err != nil {
		return 0, err
	}

	concurrency := spec.concurrencyKey
	if concurrency == "" {
		// Two runs of the same tool usually converge the same remote, so they
		// serialize by default; --concurrency-key is how a run joins a
		// workspace's or a play's key instead.
		concurrency = cliRunbookPrefix + tool.Name
	}

	queued := queue.Spec{
		Runbook:          cliRunbookPrefix + tool.Name,
		Kind:             executor.KindCLI,
		Trigger:          "manual",
		Argv:             argv,
		Dir:              root,
		DirReadOnly:      rootReadOnly,
		RunSecretBinding: spec.runSecrets,
		RunnerName:       spec.runnerName,
		Source:           spec.sourceName,
		ConcurrencyKey:   concurrency,
		Privileged:       spec.privileged,
		IdempotencyKey:   spec.idempotencyKey,
		Timeout:          spec.timeout,
	}

	if spec.requireApproval {
		return gateCLIRun(ctx, cmd, st, dir, queued, spec.json)
	}

	var final store.Job
	if daemonRunning(ctx, dir) {
		final, err = enqueueForDaemon(ctx, st, dir, queued, events)
	} else {
		final, err = cliRunInline(ctx, st, dir, tool, queued, events, warn)
	}
	if err != nil {
		return 0, err
	}
	if spec.json {
		if err := emitJSON(cmd, wire.KindJob, wire.NewJobView(final)); err != nil {
			return 0, err
		}
		// The exit-code mapping stays the one every other run verb uses; only
		// the human line it prints is dropped.
		return reportJobOutcome(io.Discard, final), nil
	}
	return reportJobOutcome(out, final), nil
}

// resolveCLITool is the allow-list check. It is what makes the kind governable:
// a name with no registration is refused here, before a job row exists, so an
// unregistered binary never reaches the queue at all.
func resolveCLITool(ctx context.Context, st *store.Store, name string) (store.CLITool, error) {
	tool, err := st.CLIToolByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.CLITool{}, fmt.Errorf(
			"run-cli: %q is not on the CLI allow-list, so it will not run: %w "+
				"(register it with `exocomp cli-tool add %s --binary <path>`, "+
				"or see the allow-list with `exocomp cli-tool list`)",
			name, store.ErrNotFound, name)
	}
	if err != nil {
		return store.CLITool{}, err
	}
	return tool, nil
}

// cliRunArgv assembles the job's argv: the tool name, the registration's fixed
// arguments, then the operator's own. Every argument passes the structural
// guard, and the operator's own also pass the registration's pattern when it
// declares one — the fixed arguments are the operator's earlier, confirmed
// decision and are not re-narrowed by it.
func cliRunArgv(tool store.CLITool, args []string) ([]string, error) {
	if err := executor.ValidateCLIArgs(args); err != nil {
		return nil, fmt.Errorf("run-cli: %w", err)
	}
	if err := executor.ValidateCLIArgs(tool.FixedArgs); err != nil {
		return nil, fmt.Errorf("run-cli: registration for %q: %w", tool.Name, err)
	}

	matcher, err := tool.ArgMatcher()
	if err != nil {
		return nil, fmt.Errorf("run-cli: %w", err)
	}
	if matcher != nil {
		for _, a := range args {
			if !matcher.MatchString(a) {
				return nil, fmt.Errorf("run-cli: argument %q does not match the pattern %q that %q is registered with",
					a, tool.ArgPattern, tool.Name)
			}
		}
	}

	full := slices.Concat(tool.FixedArgs, args)
	return executor.CLIArgv(tool.Name, full...), nil
}

// gateCLIRun routes a --require-approval CLI run through the one approval gate.
// The job lands in awaiting_approval, where no worker can claim it, and waits
// for a decision.
func gateCLIRun(ctx context.Context, cmd *cobra.Command, st *store.Store, dir string,
	spec queue.Spec, asJSON bool) (int, error) {
	job, approval, err := gateJob(ctx, st, dir, spec)
	if err != nil {
		return 0, fmt.Errorf("run-cli: %w", err)
	}
	if asJSON {
		return 0, emitJSON(cmd, wire.KindJob, wire.NewJobViewResolved(ctx, st, job))
	}
	printGatedJob(cmd.OutOrStdout(), job, approval)
	return 0, nil
}

// cliRunInline enqueues the job and drains it in this process. The bound
// run-secrets are resolved here, held for the length of the run, and added to
// what the redactor scrubs — the same shape a no-daemon playbook run has.
func cliRunInline(ctx context.Context, st *store.Store, dir string, tool store.CLITool,
	spec queue.Spec, out, warn io.Writer) (store.Job, error) {
	var env *secrets.Envelope
	if opened, err := secrets.Open(dir); err != nil {
		_, _ = fmt.Fprintf(warn, "WARNING: no envelope in %s (%v): running without bound "+
			"run-secrets and without redaction\n", dir, err)
	} else {
		env = opened
		defer func() { _ = env.Close() }()
	}

	runSecrets, secretPlain, err := resolveRunSecrets(ctx, st, env, spec.RunSecretBinding, "")
	if err != nil {
		return store.Job{}, err
	}
	if spec.RunSecretBinding != "" && len(runSecrets) == 0 {
		return store.Job{}, fmt.Errorf("run-cli: --run-secrets %q resolved to nothing; "+
			"the run would authenticate with no credential", spec.RunSecretBinding)
	}

	caps, err := resolveRunnerCaps(ctx, st, spec.RunnerName)
	if err != nil {
		return store.Job{}, fmt.Errorf("run-cli: %w", err)
	}
	driver, err := cliTierDriver(dir, executor.CLIOptions{
		Tool:       tool.Name,
		Binary:     tool.Binary,
		RunSecrets: runSecrets,
		AllowHTTPS: tool.Egress == store.CLIEgressHTTPS,
	}, caps)
	if err != nil {
		return store.Job{}, fmt.Errorf("run-cli: %w", err)
	}

	var qopts []queue.Option
	if env != nil {
		known := append(knownSecrets(ctx, st, env, warn), secretPlain...)
		qopts = append(qopts, queue.WithRedactor(secrets.NewRedactor(known)))
	}

	q := queue.New(st, driver, queue.Options{
		Workers:      1,
		PollInterval: 50 * time.Millisecond,
	}, qopts...)

	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		return store.Job{}, err
	}
	if err := drainAndTail(ctx, q, st, job.ID, out); err != nil {
		return store.Job{}, err
	}
	return st.GetJob(ctx, job.ID)
}

// cliTierDriver returns the CLI driver for the effective tier, with caps
// applied when non-empty. Under the sandbox the private directory moves
// inside the 0700 data dir, which is where every other sandboxed kind puts
// it: the system temp dir is somewhere every other process on the host can
// also name.
func cliTierDriver(dataDir string, opts executor.CLIOptions, caps executor.Caps) (executor.Driver, error) {
	if !sandboxed(context.Background()) {
		if err := requireSandboxedForCaps(context.Background(), caps); err != nil {
			return nil, err
		}
		return executor.NewCLI(opts), nil
	}
	if opts.PrivateDirRoot == "" {
		root := filepath.Join(dataDir, privateRunDir)
		if err := os.MkdirAll(root, 0o700); err == nil {
			opts.PrivateDirRoot = root
		}
	}
	return executor.NewSandboxedCLI(opts, withCaps(sandboxOptions(dataDir), caps, os.Stderr)), nil
}
