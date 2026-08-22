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
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

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
		timeout      time.Duration
		wait         bool
	)
	run := &cobra.Command{
		Use:   "run <workspace> <playbook>",
		Short: "Define and start an apply->play handoff workflow",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoff(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), handoffSpec{
				workspace:    args[0],
				playbook:     args[1],
				vaultProject: vaultProject,
				runSecrets:   runSecrets,
				timeout:      timeout,
				wait:         wait,
			})
		},
	}
	run.Flags().StringVar(&vaultProject, "vault-project", "",
		"derive the ansible-vault password for the play from the envelope")
	run.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as provider/TF_VAR_ env for the apply step")
	run.Flags().DurationVar(&timeout, "timeout", 0, "per-step timeout (0: no limit)")
	run.Flags().BoolVar(&wait, "wait", false, "follow the workflow until it reaches a terminal state")
	return run
}

func newHandoffStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <workflow-id>",
		Short: "Print a handoff workflow's steps from the ledger",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ctx := cmd.Context()
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()
			return printWorkflow(ctx, st, cmd.OutOrStdout(), args[0])
		},
	}
}

type handoffSpec struct {
	workspace    string
	playbook     string
	vaultProject string
	runSecrets   string
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

	seq := queue.NewSequencer(queue.New(st, enqueueOnly{}, queue.Options{Workers: 1}))
	if err := seq.Start(ctx, workflowID, steps); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "workflow %s started: apply %s -> play %s\n",
		workflowID, ws.Name, playbook)

	if !hs.wait {
		_, _ = fmt.Fprintf(out, "follow it with `exocomp handoff status %s`\n", workflowID)
		return nil
	}
	return followWorkflow(ctx, st, out, dir, workflowID)
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

// printWorkflow renders a workflow's steps from the ledger: the step, its kind,
// its status, and the job that ran it. A blocked step is the visible proof that
// a failed predecessor retired the rest.
func printWorkflow(ctx context.Context, st *store.Store, out io.Writer, workflowID string) error {
	steps, err := st.GetWorkflow(ctx, workflowID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("handoff: no workflow %q", workflowID)
	}
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
