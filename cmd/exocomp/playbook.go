package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newPlaybookCmd)
}

func newPlaybookCmd() *cobra.Command {
	var (
		inventory    string
		vaultProject string
		runSecrets   string
		runnerName   string
		sourceName   string
		timeout      time.Duration
		wait         bool
		limit        string
		tags         string
		skipTags     string
		extraVars    []string
		check        bool
		diff         bool
		tolerateUnre bool
		noFollow     bool
		privileged   bool
	)

	run := &cobra.Command{
		Use:   "run <playbook>",
		Short: "Run one playbook through the queue",
		Long: "Enqueues the playbook as an ansible job, streaming its events. A running " +
			"`exocomp serve` claims and runs the job; with no daemon up the queue is " +
			"drained in this process instead. The vault password is derived from the " +
			"envelope for --vault-project by whichever process runs it, and handed to " +
			"ansible through a file in a private directory that is removed when the run " +
			"ends. The process exits with the job's exit code.\n\n" +
			"--limit, --tags, --skip-tags, --check and --diff pass straight through to " +
			"ansible-playbook. --extra-var/-e is repeatable and takes either key=val or " +
			"@file: a key=val is carried on the job row and written to a file for the " +
			"run, never onto ansible's own argv; @file is passed through as -e @path. " +
			"--check runs a dry run and emits the structured config-drift outcome as a " +
			"job event (see docs/operations/playbook-run.md).\n\n" +
			"With --source, the play runs from that source's checkout and honours its " +
			"`.exocomp/hooks.yml`: if the source declares pre/post-run hooks or a health " +
			"gate the run becomes the same lifecycle workflow `exocomp run` composes " +
			"(and needs a running daemon). A source with no hooks runs exactly as above.\n\n" +
			"With --remote the play is enqueued on that daemon over HTTP and this command " +
			"follows the job's event stream until it settles, exiting with the job's code " +
			"just as a local run does; --no-follow prints the job id instead. Inputs are " +
			"resolved on the daemon, so name a --source and a registered --inventory rather " +
			"than paths on this machine.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, warn := cmd.OutOrStdout(), cmd.ErrOrStderr()

			rc, remote, err := remoteClient(cmd)
			if err != nil {
				return err
			}
			if privileged && remote {
				return errors.New("playbook run: --privileged applies to a local run; " +
					"a --remote run's tier is the daemon's, set on that host")
			}
			if privileged {
				warnPrivileged(warn)
			}
			if remote {
				// The remote run enqueues over HTTP and follows the job the daemon
				// runs; the ansible flags are split by the SAME service verb there.
				return exitWith(runRemotePlaybook(cmd, rc, remotePlaybookOp{
					playbook: args[0], inventory: inventory, vaultProject: vaultProject,
					runSecrets: runSecrets, source: sourceName, limit: limit, tags: tags,
					skipTags: skipTags, extraVars: extraVars, check: check, diff: diff,
					tolerateUnreachable: tolerateUnre, wait: wait, noFollow: noFollow,
					timeout: timeout, idempotencyKey: idempotencyKey(cmd),
					runnerName: runnerName,
				}))
			}
			if noFollow {
				return errors.New("playbook run: --no-follow applies to a --remote run; " +
					"a local run drains or follows the job it enqueues")
			}

			passthrough, vars, err := ansiblePassthrough(limit, tags, skipTags, check, diff, extraVars)
			if err != nil {
				return err
			}

			// A named source's lifecycle applies no matter which command runs the
			// play: when it declares hooks or a gate, defer to the one workflow
			// composition `exocomp run` owns rather than the plain queued play.
			var sourceDir string
			var sourceReadOnly bool
			if sourceName != "" {
				hooked, pb, root, readOnly, err := sourcePlaybook(cmd.Context(), warn, sourceName, args[0])
				if err != nil {
					return err
				}
				if hooked {
					return runWithHooks(cmd.Context(), out, warn, runSpec{
						sourceName:          sourceName,
						playbook:            args[0],
						inventory:           inventory,
						vaultProject:        vaultProject,
						runSecrets:          runSecrets,
						runnerName:          runnerName,
						timeout:             timeout,
						wait:                wait,
						passthrough:         passthrough,
						extraVars:           vars,
						tolerateUnreachable: tolerateUnre,
						privileged:          privileged,
					})
				}
				args[0] = pb
				// The run executes from the source checkout so its ./ansible.cfg
				// (and the relative inventory, roles, collections that file names)
				// resolve, with the source's read-only posture carried through —
				// the same working-directory grant the lifecycle `exocomp run` path
				// gives its play step.
				sourceDir, sourceReadOnly = root, readOnly
			}

			code, err := runPlaybook(cmd.Context(), out, warn, playbookSpec{
				playbook:            args[0],
				inventory:           inventory,
				vaultProject:        vaultProject,
				runSecrets:          runSecrets,
				runnerName:          runnerName,
				sourceName:          sourceName,
				timeout:             timeout,
				passthrough:         passthrough,
				extraVars:           vars,
				tolerateUnreachable: tolerateUnre,
				idempotencyKey:      idempotencyKey(cmd),
				dir:                 sourceDir,
				dirReadOnly:         sourceReadOnly,
				privileged:          privileged,
			})
			if err != nil {
				return err
			}
			if code != 0 {
				// A cobra RunE cannot carry an exit code; runPlaybook has
				// already released the store and the envelope by here.
				os.Exit(code)
			}
			return nil
		},
	}
	run.Flags().StringVar(&inventory, "inventory", "",
		"inventory path, or a registered inventory name (empty: ansible discovers its own)")
	run.Flags().StringVar(&vaultProject, "vault-project", "",
		"derive the ansible-vault password for this project from the envelope")
	run.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as named env for the run (a dynamic-inventory plugin or module authenticates from it)")
	run.Flags().StringVar(&runnerName, "runner", "",
		"enforce this registered runner's memory_max/cpu_quota as a per-job cgroup cap "+
			"(local-sandboxed only; refused, never run uncapped, if it cannot be enforced)")
	run.Flags().StringVar(&sourceName, "source", "",
		"run from this registered source's checkout and honour its .exocomp/hooks.yml")
	run.Flags().DurationVar(&timeout, "timeout", 0,
		"kill the run after this long (0: no limit)")
	run.Flags().BoolVar(&wait, "wait", false,
		"when the source has hooks, follow the composed workflow until it is terminal")
	run.Flags().StringVar(&limit, "limit", "",
		"ansible --limit: restrict the play to this host pattern")
	run.Flags().StringVar(&tags, "tags", "",
		"ansible --tags: only run tasks with these tags")
	run.Flags().StringVar(&skipTags, "skip-tags", "",
		"ansible --skip-tags: skip tasks with these tags")
	run.Flags().StringArrayVarP(&extraVars, "extra-var", "e", nil,
		"repeatable: key=val (written to a run-private file, never ansible's argv) or @file")
	run.Flags().BoolVar(&check, "check", false,
		"ansible --check: dry run; emits the structured config-drift outcome as a job event")
	run.Flags().BoolVar(&diff, "diff", false,
		"ansible --diff: show what would change (pairs with --check)")
	run.Flags().BoolVar(&noFollow, "no-follow", false,
		"with --remote: print the enqueued job id and exit 0 instead of following the job to its end")
	run.Flags().BoolVar(&tolerateUnre, "tolerate-unreachable", false,
		"a run whose only non-success is unreachable hosts settles degraded, not failed, and does not page (a task failure on a reachable host still fails)")
	run.Flags().BoolVar(&privileged, "privileged", false,
		"run this job UNSANDBOXED — full host access, no kernel sandbox; operator only (see docs/security/threat-model.md)")

	cmd := &cobra.Command{
		Use:   "playbook",
		Short: "Run ansible playbooks through the queue",
	}
	cmd.AddCommand(run, newPlaybookVendorCmd())
	return cmd
}

// warnPrivileged prints the one-line, loud notice that a run was asked to skip
// the sandbox. It is stderr, not a job event: the ledger record is written by
// the service and the queue; this is the operator's own immediate confirmation
// that they opted a run out of the sandbox at the keyboard.
func warnPrivileged(warn io.Writer) {
	_, _ = fmt.Fprintln(warn, "WARNING: --privileged: this run executes UNSANDBOXED "+
		"(no kernel sandbox, full host access). Operator-only, audited.")
}

type playbookSpec struct {
	playbook     string
	inventory    string
	vaultProject string
	runSecrets   string
	runnerName   string
	// sourceName names the registered source this run executes from, carried
	// onto the job row for display; empty on the raw-path form.
	sourceName string
	timeout    time.Duration
	// dir is the run's working directory: a --source run's checkout root, so
	// ansible loads that tree's own ./ansible.cfg (and its relative inventory,
	// roles, collections). Empty on the raw-path form, where the executor picks
	// the working directory. dirReadOnly carries the source's read-only posture.
	dir         string
	dirReadOnly bool
	// passthrough is the public ansible flags (--limit, --tags, --skip-tags,
	// --check, --diff, -e @file) placed before the playbook in argv.
	passthrough []string
	// extraVars are `-e key=val` variables, kept off argv and written to a
	// run-private file by the ansible driver.
	extraVars map[string]string
	// tolerateUnreachable opts the run into the degraded outcome.
	tolerateUnreachable bool
	// privileged runs the job unsandboxed (the disabled tier), full host access.
	privileged bool
	// idempotencyKey dedups a retried run to one job; empty on the unchanged path.
	idempotencyKey string
}

// ansiblePassthrough splits the operator's flags into the argv passthrough and
// the extra-vars map. The validation and injection guard live in the shared
// service verb, so a CLI run and a server-side run split an operator's flags
// identically; this wrapper keeps the CLI call site and its signature.
func ansiblePassthrough(limit, tags, skipTags string, check, diff bool, extraVars []string) ([]string, map[string]string, error) {
	return service.AnsiblePassthrough(limit, tags, skipTags, check, diff, extraVars)
}

// runPlaybook returns the job's exit code, and an error only when the job
// could not be run at all. It resolves the run's inventory, then hands the
// parsed input to the service verb, which assembles the spec and enqueues it
// through the chosen Enqueuer; the CLI keeps the streaming and the exit-code
// mapping.
func runPlaybook(ctx context.Context, out, warn io.Writer, spec playbookSpec) (code int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	playbook, err := filepath.Abs(spec.playbook)
	if err != nil {
		return 0, fmt.Errorf("playbook: resolve %s: %w", spec.playbook, err)
	}

	dir, err := dataDir()
	if err != nil {
		return 0, fmt.Errorf("playbook: resolve data dir: %w", err)
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

	// --inventory names either a registered inventory (whose env_binding is
	// resolved for a dynamic plugin) or a plain path.
	inventory, invName, err := resolveInventoryArg(ctx, st, dir, spec.inventory)
	if err != nil {
		return 0, err
	}

	var enq service.Enqueuer
	if daemonRunning(ctx, dir) {
		enq = daemonEnqueuer{st: st, dir: dir, out: out}
	} else {
		enq = playbookInlineEnqueuer{st: st, dir: dir, out: out, warn: warn}
	}

	res, err := service.New(st, dir, nil).RunPlaybook(ctx, enq, service.PlaybookInput{
		PlaybookPath:        playbook,
		InventoryPath:       inventory,
		InventoryName:       invName,
		VaultProject:        spec.vaultProject,
		RunSecrets:          spec.runSecrets,
		Passthrough:         spec.passthrough,
		ExtraVars:           spec.extraVars,
		TolerateUnreachable: spec.tolerateUnreachable,
		IdempotencyKey:      spec.idempotencyKey,
		Timeout:             spec.timeout,
		RunnerName:          spec.runnerName,
		Source:              spec.sourceName,
		Dir:                 spec.dir,
		DirReadOnly:         spec.dirReadOnly,
		Privileged:          spec.privileged,
	})
	if err != nil {
		return 0, err
	}
	return reportJobOutcome(out, res.Job), nil
}

// daemonEnqueuer hands a spec to a running daemon and follows the job it creates
// to a terminal state; the CLI does not drain, because two queues over one table
// race for the row. Its EnqueueGated is an enqueue-only insert — a gated job
// waits for a decision and is never driven here.
type daemonEnqueuer struct {
	st  *store.Store
	dir string
	out io.Writer
}

func (d daemonEnqueuer) Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error) {
	return enqueueForDaemon(ctx, d.st, d.dir, spec, d.out)
}

func (d daemonEnqueuer) EnqueueGated(ctx context.Context, spec queue.Spec) (store.Job, error) {
	return queue.New(d.st, enqueueOnly{}, queue.Options{Workers: 1}).EnqueueGated(ctx, spec)
}

// playbookInlineEnqueuer drains the job in this process, which is what a host
// with no daemon does. A playbook never gates, so EnqueueGated is present only
// to satisfy the interface.
type playbookInlineEnqueuer struct {
	st        *store.Store
	dir       string
	out, warn io.Writer
}

func (e playbookInlineEnqueuer) Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error) {
	return runPlaybookInline(ctx, e.st, e.dir, spec, e.out, e.warn)
}

func (e playbookInlineEnqueuer) EnqueueGated(ctx context.Context, spec queue.Spec) (store.Job, error) {
	return queue.New(e.st, enqueueOnly{}, queue.Options{Workers: 1}).EnqueueGated(ctx, spec)
}

// reportJobOutcome prints the terminal job's one-line summary and maps it to a
// shell exit code. A degraded success (an unreachable-tolerant run that settled
// partial) is a success: it prints "(degraded)" and exits 0, so a tolerated
// offline host is not a failure to the caller or to whatever watches the code.
func reportJobOutcome(out io.Writer, final store.Job) int {
	stateLabel := string(final.State)
	if final.Partial {
		stateLabel += " (degraded)"
	}
	_, _ = fmt.Fprintf(out, "job %d  state: %s  duration: %s\n",
		final.ID, stateLabel, jobDuration(final))

	switch {
	case final.State == store.StateSucceeded:
		return 0
	case final.ExitCode != nil && *final.ExitCode != 0:
		return *final.ExitCode
	default:
		return 1
	}
}

// sourcePlaybook resolves whether a --source run is a hooked lifecycle and,
// when it is not, the playbook path rebased onto the source checkout plus the
// checkout root and its read-only posture — so a plain play runs from the
// source's own tree, its working directory set to that root, and loads the
// tree's ./ansible.cfg (and the relative inventory, roles, collections it
// names). The hooked case is handled by runWithHooks, which rebases the path
// and sets the working directory itself.
//
// The rebase delegates to internal/service.ResolveSourcePlaybook, the same verb
// a --remote run resolves through, so a local CLI run confines the playbook to
// the source checkout identically to the daemon: a bare filepath.Join here
// would let `../` or an absolute path escape it. The root and read-only posture
// come from sourceCheckoutRoot, the helper the run-cli and ad-hoc paths resolve
// their working directory through.
func sourcePlaybook(ctx context.Context, _ io.Writer, sourceName, playbook string) (hooked bool, abs, root string, readOnly bool, err error) {
	dir, err := dataDir()
	if err != nil {
		return false, "", "", false, fmt.Errorf("source playbook: resolve data dir: %w", err)
	}
	st, err := openStore()
	if err != nil {
		return false, "", "", false, err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	root, readOnly, err = sourceCheckoutRoot(ctx, st, dir, sourceName, "playbook run")
	if err != nil {
		return false, "", "", false, err
	}
	hooked, abs, err = service.New(st, dir, nil).ResolveSourcePlaybook(ctx, hooksOverridePath(dir, sourceName), sourceName, playbook)
	return hooked, abs, root, readOnly, err
}

// resolveInventoryArg turns a --inventory value into the path ansible targets
// and, when it names a registered inventory, that inventory's name so its
// env_binding resolves for a dynamic plugin. The transport-free resolution lives
// in internal/service, so a CLI run and a server-side run turn an operator's
// names into the same paths; this wrapper adapts the CLI's explicit store+dir.
func resolveInventoryArg(ctx context.Context, st *store.Store, dataDir, value string) (path, name string, err error) {
	return service.New(st, dataDir, nil).ResolveInventory(ctx, value)
}

// runPlaybookInline enqueues the job and drains it in this process, which is
// what a host with no daemon does. The vault password is derived here, held
// for the length of the run, and added to what the redactor scrubs.
func runPlaybookInline(ctx context.Context, st *store.Store, dir string, spec queue.Spec,
	out, warn io.Writer) (store.Job, error) {
	var env *secrets.Envelope
	if opened, err := secrets.Open(dir); err != nil {
		_, _ = fmt.Fprintf(warn, "WARNING: no envelope in %s (%v): running without a vault "+
			"password and without redaction\n", dir, err)
	} else {
		env = opened
		defer func() { _ = env.Close() }()
	}

	runSecrets, secretPlain, err := resolveRunSecrets(ctx, st, env, spec.RunSecretBinding, spec.Inventory)
	if err != nil {
		return store.Job{}, err
	}

	inventoryArtifact := ""
	if !hasInventoryFlag(spec.Argv) {
		inventoryArtifact, err = staticInventoryPath(ctx, st, spec.Inventory)
		if err != nil {
			return store.Job{}, err
		}
	}
	// Resolve the per-source venv (ADR 0011 vendoring step) before draining the
	// job inline. Fail-closed: a manifest that will not resolve stops the run.
	venv, err := resolveProjectVenv(ctx, dir, ansiblePlaybookPath(spec.Argv))
	if err != nil {
		return store.Job{}, err
	}
	// Resolve the per-source ansible collections (ADR 0015 vendoring step) the
	// same way; the shared cache/collections dir stays as the fallback.
	collections, err := resolveSourceCollections(ctx, dir, ansiblePlaybookPath(spec.Argv))
	if err != nil {
		return store.Job{}, err
	}
	caps, err := resolveRunnerCaps(ctx, st, spec.RunnerName)
	if err != nil {
		return store.Job{}, err
	}
	// The daemonless play path declares no captures: the bootstrap chain's capture
	// seal runs only through the daemon's ansibleJobDriver, so this passes none.
	driver, derived, err := newAnsibleDriver(dir, env, spec.VaultProject, runSecrets, inventoryArtifact, venv, collections, spec.ExtraVars, spec.TolerateUnreachable, nil, nil, caps)
	if err != nil {
		return store.Job{}, err
	}
	var qopts []queue.Option
	if env != nil {
		known := append(knownSecrets(ctx, st, env, warn), derived...)
		known = append(known, secretPlain...)
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

// knownSecrets collects the plaintexts the redactor must scrub. A secret that
// will not open is reported by name only and skipped: an unreadable row must
// not stop a run.
func knownSecrets(ctx context.Context, st *store.Store, env *secrets.Envelope, warn io.Writer) [][]byte {
	rows, err := st.ListSecrets(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(warn, "WARNING: list secrets for redaction: %v\n", err)
		return nil
	}

	known := make([][]byte, 0, len(rows))
	for _, row := range rows {
		pt, err := env.OpenSecret(secrets.SealedSecret{
			Name:       row.Name,
			Kind:       row.Kind,
			Nonce:      row.Nonce,
			Ciphertext: row.Ciphertext,
		})
		if err != nil {
			_, _ = fmt.Fprintf(warn, "WARNING: secret %q not opened for redaction\n", row.Name)
			continue
		}
		known = append(known, pt)
	}
	return known
}

// drainAndTail runs the queue to completion while printing the job's events
// as they land.
func drainAndTail(ctx context.Context, q *queue.Queue, st *store.Store, jobID int64, out io.Writer) error {
	done := make(chan error, 1)
	go func() { done <- q.Drain(ctx) }()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	seen := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				return err
			}
			_, tailErr := tailEvents(ctx, st, jobID, seen, out)
			return tailErr
		case <-ticker.C:
			n, err := tailEvents(ctx, st, jobID, seen, out)
			if err != nil {
				return err
			}
			seen = n
		}
	}
}

// tailEvents prints the events after the first seen of them and returns the
// new count.
func tailEvents(ctx context.Context, st *store.Store, jobID int64, seen int, out io.Writer) (int, error) {
	events, err := st.ListJobEvents(ctx, jobID)
	if err != nil {
		return seen, err
	}
	for _, e := range events[min(seen, len(events)):] {
		_, _ = fmt.Fprintf(out, "[%s] %s\n", e.Level, e.Msg)
	}
	return len(events), nil
}
