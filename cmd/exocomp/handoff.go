package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/verify"
	"exocomp.dev/exocomp/internal/wire"
)

// notFoundError lets a hand-written "no such X" message still classify as
// wire.CodeNotFound (process exit wire.ExitNotFound): its Unwrap resolves to
// the store sentinel even though Error() never repeats the sentinel's own
// text, so exitCodeFor sees store.ErrNotFound without the text a human reads
// changing at all. The same technique already backs
// internal/service.PlanUnadjudicatedError. Shared by handoff.go and hooks.go,
// whose "no such <thing>" errors both predate --output json and both need
// the same fix.
type notFoundError struct{ msg string }

func (e notFoundError) Error() string { return e.msg }
func (e notFoundError) Unwrap() error { return store.ErrNotFound }

// notFoundf builds a friendly not-found error whose exit code is correct on
// both the text and JSON paths.
func notFoundf(format string, a ...any) error {
	return notFoundError{msg: fmt.Sprintf(format, a...)}
}

func init() {
	registerCommand(newHandoffCmd)
}

// inventoryArtifactKind labels the materialized inventory on the apply job that
// produced it.
const inventoryArtifactKind = "inventory"

// handoffInventoryPrefix namespaces the inventory a workspace's tofu outputs
// materialize into, so the name a play targets (`tf:<workspace>`) never
// collides with an operator's own registered inventories.
const handoffInventoryPrefix = "tf:"

// handoffInventoryName is the registered inventory name a workspace's
// materialized outputs are published under.
func handoffInventoryName(workspace string) string {
	return handoffInventoryPrefix + workspace
}

// handoffInventoryPath is the deterministic file a workspace's apply writes its
// non-sensitive outputs to, inside the 0700 data dir. It is deterministic so
// the producer (the apply driver) and the consumer (the dependent play) agree
// on it without a second round-trip, and so each apply refreshes the same file.
func handoffInventoryPath(dataDir, workspace string) string {
	return filepath.Join(dataDir, "artifacts", "inventory", workspace+".json")
}

func newHandoffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Run the Terraform->Ansible inventory handoff (story 5)",
		Long: "Chains a tofu apply to an ansible play as one step-ledger workflow: the " +
			"apply captures the workspace's declared, non-sensitive outputs into an " +
			"inventory artifact, and the play consumes exactly that artifact — from its " +
			"own 0700 private dir, with no state bearer, no passphrase, and no tfstate. " +
			"The chain is driven by a running `exocomp serve`; step 2 runs only if the " +
			"apply succeeded.",
	}
	cmd.AddCommand(newHandoffRunCmd(), newHandoffStatusCmd())
	return cmd
}

func newHandoffRunCmd() *cobra.Command {
	var (
		vaultProject string
		runSecrets   string
		sourceName   string
		timeout      time.Duration
		wait         bool
	)
	run := &cobra.Command{
		Use:   "run <workspace> <playbook>",
		Short: "Define and start an apply->play handoff workflow",
		Long: "Chains the workspace's apply to the play as one step-ledger workflow. With " +
			"--source, the source's `.exocomp/hooks.yml` wraps the chain: its pre_run " +
			"hooks run before the apply, its post_run hooks and health gate after the " +
			"play, so a source's lifecycle applies to the handoff exactly as it does to " +
			"`exocomp run`. A source with no hooks leaves the apply->play chain unchanged.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoff(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), handoffSpec{
				workspace:    args[0],
				playbook:     args[1],
				vaultProject: vaultProject,
				runSecrets:   runSecrets,
				sourceName:   sourceName,
				timeout:      timeout,
				wait:         wait,
			})
		},
	}
	run.Flags().StringVar(&vaultProject, "vault-project", "",
		"derive the ansible-vault password for the play from the envelope")
	run.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as provider/TF_VAR_ env for the apply step")
	run.Flags().StringVar(&sourceName, "source", "",
		"wrap the chain in this registered source's .exocomp/hooks.yml lifecycle")
	run.Flags().DurationVar(&timeout, "timeout", 0, "per-step timeout (0: no limit)")
	run.Flags().BoolVar(&wait, "wait", false, "follow the workflow until it reaches a terminal state")
	return run
}

func newHandoffStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "status <workflow-id>",
		Short:   "Print a handoff workflow's steps from the ledger",
		Example: "exocomp handoff status handoff:web:1700000000 --output json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			st, err := openStore()
			if err != nil {
				if mode == outputJSON {
					return failJSON(cmd, err)
				}
				return err
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()
			if mode == outputJSON {
				steps, gerr := getWorkflowSteps(ctx, st, args[0])
				if gerr != nil {
					return failJSON(cmd, gerr)
				}
				return emitJSON(cmd, wire.KindWorkflow, wire.NewWorkflowView(args[0], steps))
			}
			return printWorkflow(ctx, st, cmd.OutOrStdout(), args[0])
		},
	}
}

type handoffSpec struct {
	workspace    string
	playbook     string
	vaultProject string
	runSecrets   string
	sourceName   string
	timeout      time.Duration
	wait         bool
}

// runHandoff defines the apply->play chain as a ledger workflow and starts it.
// The workflow's jobs are gated at Start: the daemon's sequencer releases step
// 1, and step 2 only once step 1 succeeded. The daemon is the sole executor —
// this command records the workflow and, with --wait, follows it.
func runHandoff(ctx context.Context, out, warn io.Writer, hs handoffSpec) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	playbook, err := filepath.Abs(hs.playbook)
	if err != nil {
		return fmt.Errorf("handoff: resolve %s: %w", hs.playbook, err)
	}

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("handoff: resolve data dir: %w", err)
	}

	st, err := openJobStore(ctx, warn)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	ws, err := st.WorkspaceByName(ctx, hs.workspace)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("handoff: no workspace %q", hs.workspace)
	}
	if err != nil {
		return err
	}

	// The chain progresses only while a daemon drives the sequencer: it releases
	// each gated step, its workers run the job, and it resumes the workflow from
	// the ledger after a restart. Without one the workflow would sit gated.
	if !daemonRunning(ctx, dir) {
		return errors.New("handoff: no running daemon; start `exocomp serve` " +
			"(its sequencer drives the chain and resumes it after a restart)")
	}

	// Publish the materialized-inventory name up front, pointing at the
	// deterministic path the apply will write, so the play can resolve it and so
	// `playbook run --inventory tf:<ws>` works after the chain too.
	invName := handoffInventoryName(ws.Name)
	if _, err := st.MaterializeInventory(ctx, invName, handoffInventoryPath(dir, ws.Name)); err != nil {
		return err
	}

	workflowID := fmt.Sprintf("handoff:%s:%d", ws.Name, time.Now().UTC().UnixNano())
	steps := []queue.Step{
		{
			Kind: "tofu-apply",
			Spec: queue.Spec{
				Runbook:          workspaceRunbookPrefix + ws.Name,
				Kind:             executor.KindTofu,
				Trigger:          "handoff",
				Argv:             executor.TofuArgv("apply", "-auto-approve"),
				Dir:              ws.TofuDir,
				RunSecretBinding: hs.runSecrets,
				ConcurrencyKey:   "tofu:" + ws.Name,
				Timeout:          hs.timeout,
			},
		},
		{
			Kind: "ansible-play",
			Spec: queue.Spec{
				Runbook:      "playbook",
				Kind:         executor.KindAnsible,
				Trigger:      "handoff",
				Argv:         executor.AnsibleArtifactArgv(playbook),
				VaultProject: hs.vaultProject,
				Inventory:    invName,
				Timeout:      hs.timeout,
			},
		},
	}

	// A named source's lifecycle wraps the apply->play chain: its pre_run hooks
	// run before the apply, its post_run hooks and health gate after the play.
	// The source resolves against the already-open store, so this adds no second
	// connection; a source with no hooks leaves the two-step chain unchanged.
	if hs.sourceName != "" {
		src, serr := st.SourceByName(ctx, hs.sourceName)
		if errors.Is(serr, store.ErrNotFound) {
			return fmt.Errorf("handoff: no source %q", hs.sourceName)
		}
		if serr != nil {
			return serr
		}
		loc, serr := source.Resolve(dir, src.Kind, src.Name, src.URI)
		if serr != nil {
			return fmt.Errorf("handoff: %w", serr)
		}
		hk, serr := hooks.Resolve(loc.Root, hooksOverridePath(dir, src.Name))
		if serr != nil {
			return serr
		}
		if steps, serr = wrapWithHooks(loc.Root, hk, steps, hs.timeout); serr != nil {
			return fmt.Errorf("handoff: %w", serr)
		}
	}

	seq := queue.NewSequencer(queue.New(st, enqueueOnly{}, queue.Options{Workers: 1}))
	if err := seq.Start(ctx, workflowID, steps); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "workflow %s started: apply %s -> play %s (%d step(s))\n",
		workflowID, ws.Name, playbook, len(steps))

	if !hs.wait {
		_, _ = fmt.Fprintf(out, "follow it with `exocomp handoff status %s`\n", workflowID)
		return nil
	}
	return followWorkflow(ctx, st, out, dir, workflowID)
}

// wrapWithHooks wraps an ordered main-step list in a source's resolved
// lifecycle: [pre_run hooks] -> main... -> [post_run hooks] -> gate. It reuses
// the hook-step builder `exocomp run` owns (cmd/run.go) so all three commands
// compose a hook identically, and it appends the same declared-probe gate step.
// An empty Hooks returns main unchanged, which is what keeps a source with no
// lifecycle behaving exactly as it did before.
func wrapWithHooks(root string, hk hooks.Hooks, main []queue.Step, timeout time.Duration) ([]queue.Step, error) {
	if hk.Empty() {
		return main, nil
	}
	steps := make([]queue.Step, 0, len(hk.PreRun)+len(main)+len(hk.PostRun)+1)
	for _, h := range hk.PreRun {
		steps = append(steps, hookStep(root, h, timeout))
	}
	steps = append(steps, main...)
	for _, h := range hk.PostRun {
		steps = append(steps, hookStep(root, h, timeout))
	}
	if len(hk.Gate) > 0 {
		argv, err := verify.Encode(hk.Gate)
		if err != nil {
			return nil, fmt.Errorf("gate: %w", err)
		}
		steps = append(steps, queue.Step{
			Kind: verify.JobKind,
			Spec: queue.Spec{
				Runbook: "gate",
				Kind:    verify.JobKind,
				Trigger: "handoff",
				Argv:    argv,
				Timeout: timeout,
			},
		})
	}
	return steps, nil
}

// followWorkflow tails a workflow to a terminal state, printing each step's job
// events as they land. It is the --wait path; the daemon is what actually runs
// the steps.
func followWorkflow(ctx context.Context, st *store.Store, out io.Writer, dir, workflowID string) error {
	ticker := time.NewTicker(tailPoll)
	defer ticker.Stop()

	seen := map[int64]int{}
	for {
		steps, err := st.GetWorkflow(ctx, workflowID)
		if err != nil {
			return err
		}
		for _, step := range steps {
			if step.JobID == nil {
				continue
			}
			n, err := tailEvents(ctx, st, *step.JobID, seen[*step.JobID], out)
			if err != nil {
				return err
			}
			seen[*step.JobID] = n
		}
		if state := store.WorkflowStatus(steps); state != store.WorkflowRunning {
			_, _ = fmt.Fprintf(out, "workflow %s: %s\n", workflowID, state)
			return printWorkflow(ctx, st, out, workflowID)
		}
		if !serveLockHeld(dir) {
			return fmt.Errorf("handoff: the daemon stopped while workflow %s was running", workflowID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// getWorkflowSteps reads a workflow's step ledger, wrapping a not-found ledger
// in a friendly message while keeping the store.ErrNotFound classification
// intact (via notFoundf) — printWorkflow's text error and `handoff status
// --output json`'s exit code must agree this is exit wire.ExitNotFound, not a
// generic failure.
func getWorkflowSteps(ctx context.Context, st *store.Store, workflowID string) ([]store.WorkflowStep, error) {
	steps, err := st.GetWorkflow(ctx, workflowID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundf("handoff: no workflow %q", workflowID)
	}
	return steps, err
}

// printWorkflow renders a workflow's steps from the ledger: the step, its kind,
// its status, and the job that ran it. A blocked step is the visible proof that
// a failed predecessor retired the rest.
func printWorkflow(ctx context.Context, st *store.Store, out io.Writer, workflowID string) error {
	steps, err := getWorkflowSteps(ctx, st, workflowID)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "workflow %s: %s\n", workflowID, store.WorkflowStatus(steps))
	for _, s := range steps {
		job := "-"
		if s.JobID != nil {
			job = fmt.Sprintf("job %d", *s.JobID)
		}
		_, _ = fmt.Fprintf(out, "  step %d  %-13s %-10s %s\n", s.StepNo, s.Kind, s.Status, job)
	}
	return nil
}

// recordInventoryArtifact records the inventory a successful apply materialized:
// an artifact row on the apply job, and a refresh of the workspace's registered
// `tf:<workspace>` inventory. It reads the file the tofu driver wrote (the
// producer half lives in the executor), so a run where capture produced nothing
// records nothing. The digest is of the on-disk file, which is what a later
// consumer reads.
func recordInventoryArtifact(ctx context.Context, st *store.Store, jobID int64, workspace, dataDir string) error {
	path := handoffInventoryPath(dataDir, workspace)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	sum, size, err := fileDigest(path)
	if err != nil {
		return err
	}
	if _, err := st.CreateArtifact(ctx, store.Artifact{
		JobID:  jobID,
		Kind:   inventoryArtifactKind,
		Path:   path,
		SHA256: sum,
		Size:   size,
	}); err != nil {
		return err
	}
	_, err = st.MaterializeInventory(ctx, handoffInventoryName(workspace), path)
	return err
}

// staticInventoryPath returns the file a registered static inventory names, for
// the consumer to copy into its private dir. It returns "" for an empty name, a
// name that is not registered, or a dynamic inventory (whose target is a plugin
// config, not a file to copy).
func staticInventoryPath(ctx context.Context, st *store.Store, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	inv, err := st.InventoryByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if inv.Kind != store.InventoryStatic || inv.Path == "" {
		return "", nil
	}
	return inv.Path, nil
}

// hasInventoryFlag reports whether an ansible argv already names an inventory,
// so the driver does not also copy a registered one in on top of it. The
// handoff's play (AnsibleArtifactArgv) carries none, which is what routes it
// through the private-copy path; every other flow bakes the inventory into the
// argv and is left untouched.
func hasInventoryFlag(argv []string) bool {
	for _, a := range argv {
		if a == "-i" || a == "--inventory" || a == "--inventory-file" ||
			strings.HasPrefix(a, "--inventory=") || strings.HasPrefix(a, "-i=") {
			return true
		}
	}
	return false
}
