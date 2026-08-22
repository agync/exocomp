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

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/source"
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
		sourceName   string
		timeout      time.Duration
		wait         bool
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
			"With --source, the play runs from that source's checkout and honours its " +
			"`.exocomp/hooks.yml`: if the source declares pre/post-run hooks or a health " +
			"gate the run becomes the same lifecycle workflow `exocomp run` composes " +
			"(and needs a running daemon). A source with no hooks runs exactly as above.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, warn := cmd.OutOrStdout(), cmd.ErrOrStderr()

			// A named source's lifecycle applies no matter which command runs the
			// play: when it declares hooks or a gate, defer to the one workflow
			// composition `exocomp run` owns rather than the plain queued play.
			if sourceName != "" {
				hooked, pb, err := sourcePlaybook(cmd.Context(), warn, sourceName, args[0])
				if err != nil {
					return err
				}
				if hooked {
					return runWithHooks(cmd.Context(), out, warn, runSpec{
						sourceName:   sourceName,
						playbook:     args[0],
						inventory:    inventory,
						vaultProject: vaultProject,
						runSecrets:   runSecrets,
						timeout:      timeout,
						wait:         wait,
					})
				}
				args[0] = pb
			}

			code, err := runPlaybook(cmd.Context(), out, warn, playbookSpec{
				playbook:     args[0],
				inventory:    inventory,
				vaultProject: vaultProject,
				runSecrets:   runSecrets,
				timeout:      timeout,
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
		"inventory path, or a registered inventory name (default: localhost over the local connection)")
	run.Flags().StringVar(&vaultProject, "vault-project", "",
		"derive the ansible-vault password for this project from the envelope")
	run.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as named env for the run (a dynamic-inventory plugin or module authenticates from it)")
	run.Flags().StringVar(&sourceName, "source", "",
		"run from this registered source's checkout and honour its .exocomp/hooks.yml")
	run.Flags().DurationVar(&timeout, "timeout", 0,
		"kill the run after this long (0: no limit)")
	run.Flags().BoolVar(&wait, "wait", false,
		"when the source has hooks, follow the composed workflow until it is terminal")

	cmd := &cobra.Command{
		Use:   "playbook",
		Short: "Run ansible playbooks through the queue",
	}
	cmd.AddCommand(run, newPlaybookVendorCmd())
	return cmd
}

type playbookSpec struct {
	playbook     string
	inventory    string
	vaultProject string
	runSecrets   string
	timeout      time.Duration
}

// runPlaybook returns the job's exit code, and an error only when the job
// could not be run at all.
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
	inventory, invName, err := resolveInventoryArg(ctx, st, spec.inventory)
	if err != nil {
		return 0, err
	}

	// The vault project and binding names travel with the job, not the material
	// they resolve: a daemon that claims this row derives the same secrets.
	queued := queue.Spec{
		Runbook:          "playbook",
		Kind:             executor.KindAnsible,
		Trigger:          "manual",
		Argv:             executor.AnsibleArgv(playbook, inventory),
		VaultProject:     spec.vaultProject,
		RunSecretBinding: spec.runSecrets,
		Inventory:        invName,
		Timeout:          spec.timeout,
	}

	var final store.Job
	if daemonRunning(ctx, dir) {
		final, err = enqueueForDaemon(ctx, st, dir, queued, out)
	} else {
		final, err = runPlaybookInline(ctx, st, dir, queued, out, warn)
	}
	if err != nil {
		return 0, err
	}
	_, _ = fmt.Fprintf(out, "job %d  state: %s  duration: %s\n",
		final.ID, final.State, jobDuration(final))

	switch {
	case final.State == store.StateSucceeded:
		return 0, nil
	case final.ExitCode != nil && *final.ExitCode != 0:
		return *final.ExitCode, nil
	default:
		return 1, nil
	}
}

// resolveSourceHooks resolves a registered source's checkout root and its
// lifecycle (with the override-wins precedence). It is the shared read
// `playbook run` and `handoff run` use to decide whether a source's
// `.exocomp/hooks.yml` turns a run into a lifecycle workflow. A malformed hooks
// file is an error here, so a broken lifecycle fails before the run starts
// rather than being silently skipped.
func resolveSourceHooks(ctx context.Context, sourceName string) (hk hooks.Hooks, root string, err error) {
	dir, err := dataDir()
	if err != nil {
		return hooks.Hooks{}, "", fmt.Errorf("source hooks: resolve data dir: %w", err)
	}
	st, err := openStore()
	if err != nil {
		return hooks.Hooks{}, "", err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	src, err := st.SourceByName(ctx, sourceName)
	if errors.Is(err, store.ErrNotFound) {
		return hooks.Hooks{}, "", fmt.Errorf("no source %q", sourceName)
	}
	if err != nil {
		return hooks.Hooks{}, "", err
	}
	loc, err := source.Resolve(dir, src.Kind, src.Name, src.URI)
	if err != nil {
		return hooks.Hooks{}, "", fmt.Errorf("source hooks: %w", err)
	}
	hk, err = hooks.Resolve(loc.Root, hooksOverridePath(dir, src.Name))
	if err != nil {
		return hooks.Hooks{}, "", err
	}
	return hk, loc.Root, nil
}

// sourcePlaybook resolves whether a --source run is a hooked lifecycle and,
// when it is not, the playbook path rebased onto the source checkout so a plain
// play still runs from the source's own tree (loading its ./ansible.cfg). The
// hooked case is handled by runWithHooks, which rebases the path itself.
func sourcePlaybook(ctx context.Context, _ io.Writer, sourceName, playbook string) (hooked bool, abs string, err error) {
	hk, root, err := resolveSourceHooks(ctx, sourceName)
	if err != nil {
		return false, "", err
	}
	if !hk.Empty() {
		return true, "", nil
	}
	if !filepath.IsAbs(playbook) {
		playbook = filepath.Join(root, playbook)
	}
	return false, playbook, nil
}

// resolveInventoryArg turns a --inventory value into the path ansible targets
// and, when it names a registered inventory, that inventory's name so its
// env_binding resolves for a dynamic plugin. A value that matches no registered
// inventory is treated as a filesystem path. An empty value targets localhost.
func resolveInventoryArg(ctx context.Context, st *store.Store, value string) (path, name string, err error) {
	if value == "" {
		return "", "", nil
	}
	if inv, ierr := st.InventoryByName(ctx, value); ierr == nil {
		return inv.Path, inv.Name, nil
	} else if !errors.Is(ierr, store.ErrNotFound) {
		return "", "", ierr
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", "", fmt.Errorf("playbook: resolve inventory: %w", err)
	}
	return abs, "", nil
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
	driver, derived := newAnsibleDriver(dir, env, spec.VaultProject, runSecrets, inventoryArtifact, venv)
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
