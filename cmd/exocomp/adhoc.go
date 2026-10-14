package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newAnsibleAdhocCmd)
}

// defaultAdhocModule is what `command` runs and the fallback for `ansible`: the
// command module runs a bare program with no shell, so a pipe or a redirect is
// an explicit `--shell` (or `-m shell`) rather than an accident.
const defaultAdhocModule = "command"

// shellAdhocModule switches `command --shell` to a shell so pipes and redirects
// work, at the cost of the command module's no-shell safety.
const shellAdhocModule = "shell"

// adhocModuleName is the ansible module-name shape: a leading letter/digit/
// underscore, then letters, digits, underscores and dots (so `ansible.builtin.ping`
// is admitted). Anything else — a leading dash, a space, a shell metacharacter —
// is refused, so `-m` cannot smuggle a flag or a second token onto the argv.
var adhocModuleName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.]*$`)

// adhocSpec is one ad-hoc invocation: `ansible <pattern> -i <inv> -m <module>
// -a "<args>"`, run from a source checkout (or none) through the one queue
// under the same governance a playbook run gets.
type adhocSpec struct {
	// runbook labels the job so `command` and `ansible` are told apart in the
	// job list; both are ordinary ansible jobs.
	runbook             string
	sourceName          string
	pattern             string
	inventory           string
	module              string
	args                string
	become              bool
	vaultProject        string
	runSecrets          string
	runnerName          string
	requireApproval     bool
	tolerateUnreachable bool
	// privileged runs the job unsandboxed (the disabled tier), full host access.
	privileged bool
	timeout    time.Duration
	// sshIdentityFiles are single ssh identity key files the run may read,
	// validated and absolutized at the flag parse, so a one-off against a
	// key-only host does not have to reach for --privileged.
	sshIdentityFiles []string
	// idempotencyKey dedups a retried ad-hoc run to one job. Resolved from the
	// global flag in runAdhocExit, empty on the unchanged path.
	idempotencyKey string
	// outputMode format (text|json).
	outputMode string
}

func newAnsibleAdhocCmd() *cobra.Command {
	var (
		sourceName          string
		host                string
		limit               string
		inventory           string
		module              string
		args                string
		become              bool
		vaultProject        string
		runSecrets          string
		runnerName          string
		requireApproval     bool
		tolerateUnreachable bool
		privileged          bool
		timeout             time.Duration
		sshIdentity         []string
	)

	cmd := &cobra.Command{
		Use:   "ansible --host <pattern> -m <module> [-a <args>]",
		Short: "Run any ad-hoc ansible module against a host pattern",
		Long: "The power form of the everyday ad-hoc run: `ansible <pattern> -i <inv> " +
			"-m <module> -a \"<args>\"` for any module (service, copy, package, ping, " +
			"setup, …). `exocomp command` is the ergonomic shorthand for `-m command`. " +
			"It runs from a source's checkout (--source) against its inventory, or with no " +
			"inventory (ansible discovers its own), through the same governance a playbook " +
			"run gets: the sandbox, the vault-id, the fleet key, run-secrets, redaction, " +
			"and the one queue. --require-approval gates a destructive module behind the " +
			"approval gate; --tolerate-unreachable settles an unreachable-only run degraded " +
			"rather than failed. The module name and args are injection-guarded and no " +
			"secret is ever placed on ansible's argv (docs/operations/adhoc-and-partial.md).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pattern, err := resolveHostPattern(host, limit)
			if err != nil {
				return err
			}
			if err := guardModule(module); err != nil {
				return err
			}
			if err := guardModuleArgs(args); err != nil {
				return err
			}
			identityFiles, err := parseSSHIdentityFiles(sshIdentity)
			if err != nil {
				return err
			}
			return runAdhocExit(cmd, adhocSpec{
				runbook:             "ansible",
				sourceName:          sourceName,
				pattern:             pattern,
				inventory:           inventory,
				module:              module,
				args:                args,
				become:              become,
				vaultProject:        vaultProject,
				runSecrets:          runSecrets,
				runnerName:          runnerName,
				requireApproval:     requireApproval,
				tolerateUnreachable: tolerateUnreachable,
				privileged:          privileged,
				timeout:             timeout,
				sshIdentityFiles:    identityFiles,
			})
		},
	}
	cmd.Flags().StringVarP(&module, "module", "m", defaultAdhocModule, "ansible module to run")
	cmd.Flags().StringVarP(&args, "args", "a", "", "module arguments (the -a string)")
	addAdhocFlags(cmd, &sourceName, &host, &limit, &inventory, &become,
		&vaultProject, &runSecrets, &runnerName, &requireApproval, &tolerateUnreachable, &privileged, &timeout, &sshIdentity)
	return cmd
}

// addAdhocFlags registers the governance flags `command` and `ansible` share, so
// the two verbs stay one skeleton with one set of names.
func addAdhocFlags(cmd *cobra.Command, sourceName, host, limit, inventory *string,
	become *bool, vaultProject, runSecrets, runnerName *string, requireApproval, tolerateUnreachable, privileged *bool,
	timeout *time.Duration, sshIdentity *[]string) {
	cmd.Flags().StringVarP(sourceName, "source", "s", "", "run from this registered source's checkout")
	cmd.Flags().StringVar(host, "host", "", "ansible host pattern: a host, a group, or an expression like `fedora:!nas`")
	cmd.Flags().StringVar(limit, "limit", "", "alias for --host")
	cmd.Flags().StringVar(inventory, "inventory", "", "inventory path or registered inventory name (empty: ansible discovers its own)")
	cmd.Flags().BoolVar(become, "become", false, "run with privilege escalation (ansible --become)")
	cmd.Flags().StringVar(vaultProject, "vault-project", "", "derive the ansible-vault password for this project from the envelope")
	cmd.Flags().StringVar(runSecrets, "run-secrets", "", "bind a run_secret set as named env for the run")
	cmd.Flags().StringVar(runnerName, "runner", "", "enforce this registered runner's memory_max/cpu_quota "+
		"as a per-job cgroup cap (local-sandboxed only; refused, never run uncapped, if it cannot be enforced)")
	cmd.Flags().BoolVar(requireApproval, "require-approval", false, "gate the run behind the approval gate; it waits in awaiting_approval until a decision")
	cmd.Flags().BoolVar(tolerateUnreachable, "tolerate-unreachable", false, "a run whose only non-success is unreachable hosts settles degraded, not failed")
	cmd.Flags().BoolVar(privileged, "privileged", false,
		"run this job UNSANDBOXED — full host access, no kernel sandbox; operator only (see docs/security/threat-model.md)")
	cmd.Flags().DurationVar(timeout, "timeout", 0, "kill the run after this long (0: no limit)")
	cmd.Flags().StringSliceVar(sshIdentity, "ssh-identity", nil,
		"grant this run read access to an ssh identity key file (repeatable; each file is granted individually, never its directory)")
}

// resolveHostPattern reconciles --host and its --limit alias into the one
// pattern, and guards it: exactly one must be set, and the pattern may not start
// with a dash or carry a control character, so it cannot be read as a flag or
// split the argv.
func resolveHostPattern(host, limit string) (string, error) {
	pattern := host
	switch {
	case host != "" && limit != "" && host != limit:
		return "", errors.New("command: pass one of --host or --limit, not both")
	case host == "":
		pattern = limit
	}
	if pattern == "" {
		return "", errors.New("command: --host <pattern> is required")
	}
	if strings.HasPrefix(pattern, "-") {
		return "", fmt.Errorf("command: host pattern %q starts with a dash", pattern)
	}
	if hasControl(pattern) {
		return "", errors.New("command: host pattern contains a control character")
	}
	return pattern, nil
}

// guardModule refuses a module name that is not the plain ansible shape, so `-m`
// can carry only a module, never a flag or a whitespace-split second token.
func guardModule(module string) error {
	if !adhocModuleName.MatchString(module) {
		return fmt.Errorf("command: module %q is not a valid module name", module)
	}
	return nil
}

// guardModuleArgs refuses an -a string that starts with a dash or carries a
// control character. A module's args are `key=value` pairs; a leading dash is a
// flag trying to escape the single argv token, and a control character would
// split or truncate it.
func guardModuleArgs(args string) error {
	trimmed := strings.TrimSpace(args)
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("command: module args %q start with a dash", args)
	}
	if hasControl(args) {
		return errors.New("command: module args contain a control character")
	}
	return nil
}

// hasControl reports whether s carries a control character (NUL, a newline, or
// any other), the bytes an argv token cannot safely carry.
func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) }) >= 0
}

// runAdhocExit runs the ad-hoc spec and translates its job outcome into the
// process exit code, the same way `playbook run` does.
func runAdhocExit(cmd *cobra.Command, spec adhocSpec) error {
	mode, err := outputMode(cmd)
	if err != nil {
		return err
	}
	spec.outputMode = mode
	spec.idempotencyKey = idempotencyKey(cmd)
	if spec.privileged {
		warnPrivileged(cmd.ErrOrStderr())
	}
	code, err := runAdhoc(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), spec)
	if err != nil {
		if mode == outputJSON {
			return failJSON(cmd, err)
		}
		return err
	}
	if code != 0 {
		// A cobra RunE cannot carry an exit code; runAdhoc has already released
		// the store and the envelope by here.
		os.Exit(code)
	}
	return nil
}

// runAdhoc enqueues one ad-hoc ansible job and returns its exit code. It mirrors
// runPlaybook: a running daemon claims the row, otherwise the queue is drained
// in this process; --require-approval routes the job through the gate instead.
func runAdhoc(ctx context.Context, out, warn io.Writer, spec adhocSpec) (code int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	dir, err := dataDir()
	if err != nil {
		return 0, fmt.Errorf("command: resolve data dir: %w", err)
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

	if spec.become && !spec.privileged && isLocalHostAdhoc(spec.pattern, spec.inventory) {
		return 0, fmt.Errorf("%s: local privilege escalation with --become requires --privileged (operator-gated, unsandboxed)", spec.runbook)
	}

	root, rootReadOnly, err := sourceCheckoutRoot(ctx, st, dir, spec.sourceName, spec.runbook)
	if err != nil {
		return 0, err
	}
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			root = home
		}
	}

	invPath, invName, err := resolveInventoryArg(ctx, st, dir, spec.inventory)
	if err != nil {
		return 0, err
	}

	var passthrough []string
	if spec.become {
		passthrough = append(passthrough, "--become")
	}

	queued := queue.Spec{
		Runbook:             spec.runbook,
		Kind:                executor.KindAnsible,
		Trigger:             "manual",
		Argv:                executor.AnsibleAdhocArgv(spec.pattern, invPath, spec.module, spec.args, passthrough...),
		Dir:                 root,
		DirReadOnly:         rootReadOnly,
		VaultProject:        spec.vaultProject,
		RunSecretBinding:    spec.runSecrets,
		Inventory:           invName,
		Source:              spec.sourceName,
		TolerateUnreachable: spec.tolerateUnreachable,
		RunnerName:          spec.runnerName,
		Privileged:          spec.privileged,
		SSHIdentityFiles:    spec.sshIdentityFiles,
		IdempotencyKey:      spec.idempotencyKey,
		Timeout:             spec.timeout,
	}

	if spec.requireApproval {
		return gateAdhoc(ctx, st, dir, queued, out, spec.outputMode)
	}

	streamOut := out
	if spec.outputMode == outputJSON {
		streamOut = io.Discard
	}

	var final store.Job
	if daemonRunning(ctx, dir) {
		final, err = enqueueForDaemon(ctx, st, dir, queued, streamOut)
	} else {
		final, err = runPlaybookInline(ctx, st, dir, queued, streamOut, warn)
	}
	if err != nil {
		return 0, err
	}

	if spec.outputMode == outputJSON {
		events, err := st.ListJobEvents(ctx, final.ID)
		var lines []string
		if err == nil {
			lines = make([]string, 0, len(events))
			for _, e := range events {
				if e.Level == store.LevelSystem {
					continue
				}
				lines = append(lines, e.Msg)
			}
		}
		hosts := executor.ParseAnsibleAdhocOutput(lines)
		exitCode := 0
		if final.ExitCode != nil {
			exitCode = *final.ExitCode
		}
		view := wire.NewCommandResultView(final.ID, string(final.State), exitCode, hosts)
		if err := wire.WriteSuccess(out, wire.KindCommandResult, view); err != nil {
			return 0, err
		}
		return reportJobOutcome(io.Discard, final), nil
	}

	return reportJobOutcome(out, final), nil
}

// sourceCheckoutRoot resolves the working directory a run executes in and
// whether it is read-only: a named source's checkout (so the repo's own
// configuration applies), or empty for a run that executes in the caller's own
// directory (writable). verb prefixes the errors so each command's refusals name
// the command the operator typed.
func sourceCheckoutRoot(ctx context.Context, st *store.Store, dir, sourceName, verb string) (root string, readOnly bool, err error) {
	if sourceName == "" {
		return "", false, nil
	}
	src, err := st.SourceByName(ctx, sourceName)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, fmt.Errorf("%s: no source %q", verb, sourceName)
	}
	if err != nil {
		return "", false, err
	}
	loc, err := source.Resolve(dir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return "", false, fmt.Errorf("%s: %w", verb, err)
	}
	return loc.Root, loc.ReadOnly, nil
}

// gateJob routes a --require-approval job through the approval gate: it lands
// in awaiting_approval where no worker can claim it, and an approval that
// releases it is created. It is the same gate and approval machinery a gated
// tofu apply uses, so there is one gate, and it returns the pair rather than
// printing so every verb that gates can report in its own format.
func gateJob(ctx context.Context, st *store.Store, dir string, spec queue.Spec) (
	store.Job, store.Approval, error) {
	env, err := secrets.Open(dir)
	if err != nil {
		return store.Job{}, store.Approval{}, fmt.Errorf("--require-approval needs the envelope: %w", err)
	}
	defer func() { _ = env.Close() }()

	q := queue.New(st, enqueueOnly{}, queue.Options{Workers: 1})
	job, err := q.EnqueueGated(ctx, spec)
	if err != nil {
		return store.Job{}, store.Approval{}, err
	}

	_, hash, err := reactive.ApprovalToken(env, job.ID)
	if err != nil {
		return store.Job{}, store.Approval{}, err
	}
	now := time.Now().UTC()
	approval, err := st.CreateApproval(ctx, store.Approval{
		JobID:       job.ID,
		RequestedAt: now,
		ExpiresAt:   now.Add(reactive.DefaultApprovalTTL),
		TokenHash:   hash,
	})
	if err != nil {
		return store.Job{}, store.Approval{}, err
	}
	return job, approval, nil
}

// printGatedJob is the shared text report for a job that landed in the gate.
func printGatedJob(out io.Writer, job store.Job, approval store.Approval) {
	_, _ = fmt.Fprintf(out, "job %d gated (awaiting_approval); approval %d expires %s\n",
		job.ID, approval.ID, approval.ExpiresAt.Format("2006-01-02 15:04:05Z"))
	_, _ = fmt.Fprintf(out, "it waits until decided; print its decision link with `exocomp approval link %d`\n", approval.ID)
}

// gateAdhoc gates an ad-hoc ansible job and points at its decision link.
func gateAdhoc(ctx context.Context, st *store.Store, dir string, spec queue.Spec, out io.Writer, mode string) (int, error) {
	job, approval, err := gateJob(ctx, st, dir, spec)
	if err != nil {
		return 0, fmt.Errorf("command: %w", err)
	}
	if mode == outputJSON {
		return 0, wire.WriteSuccess(out, wire.KindApproval, wire.NewApprovalView(approval))
	}
	printGatedJob(out, job, approval)
	return 0, nil
}

// isLocalHostAdhoc reports whether an ad-hoc invocation exclusively targets localhost.
func isLocalHostAdhoc(pattern, inventory string) bool {
	p := strings.TrimSpace(pattern)
	inv := strings.TrimSpace(inventory)
	if inv == "localhost," || inv == "127.0.0.1," {
		return true
	}
	return (p == "localhost" || p == "127.0.0.1" || p == "::1") && (inv == "" || inv == "localhost," || inv == "127.0.0.1,")
}
