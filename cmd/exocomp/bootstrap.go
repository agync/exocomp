package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newBootstrapCmd)
}

// bootstrapTrigger labels every job the ansible-first chain enqueues.
const bootstrapTrigger = "bootstrap"

// bootstrapWorkflowKind is the wire envelope kind `--output json` emits when the
// chain is composed and started.
const bootstrapWorkflowKind = "bootstrap.workflow"

func newBootstrapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Run an Ansible-first bootstrap->apply chain",
		Long: "Composes the mirror of the handoff on the same step-ledger Sequencer: the " +
			"ansible bootstrap play runs FIRST, then `tofu apply` runs against the " +
			"workspace, as one resumable workflow driven by a running `exocomp serve`. It " +
			"exposes the ordering the Sequencer already supports for the case where the " +
			"tofu step manages resources the play prepared (kubeadm nodes, a bootstrapped " +
			"control plane). With --capture the play's fetched credential is sealed and " +
			"handed to the apply as env (ab:<workflow>); without it the apply reads its " +
			"own pre-existing --run-secrets binding.",
	}
	cmd.AddCommand(newBootstrapRunCmd(), newBootstrapStatusCmd())
	return cmd
}

func newBootstrapRunCmd() *cobra.Command {
	var (
		inventory        string
		vaultProject     string
		runSecrets       string
		captureSpecs     []string
		captureFileSpecs []string
		sourceName       string
		requireApproval  bool
		timeout          time.Duration
		wait             bool
	)
	run := &cobra.Command{
		Use:   "run <playbook> <workspace>",
		Short: "Define and start a bootstrap-play->apply workflow",
		Long: "Chains the bootstrap play to the workspace's apply as one step-ledger " +
			"workflow, play first. The play runs against --inventory (empty defers to " +
			"ansible's own inventory discovery — there is no tofu output yet to build " +
			"one from); the apply runs " +
			"only if the play succeeded. With --source, the source's .exocomp/hooks.yml " +
			"wraps the chain exactly as it wraps `handoff run`. --require-approval gates " +
			"the destructive apply behind the confirmation contract.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			captures, err := parseCaptures(captureSpecs, false)
			if err != nil {
				return err
			}
			// A --capture-file is the same capture with a file delivery: the sealed
			// value reaches the apply as a KUBECONFIG path rather than an env value.
			fileCaptures, err := parseCaptures(captureFileSpecs, true)
			if err != nil {
				return err
			}
			captures = append(captures, fileCaptures...)
			// The apply carries one run-secret binding. With any capture that slot is
			// the chain's own ab:<workflow>; a pre-existing --run-secrets would have
			// to share it, which one field cannot, so the two are refused together
			// rather than one silently winning.
			if len(captures) > 0 && runSecrets != "" {
				return errors.New("bootstrap: --capture/--capture-file and --run-secrets " +
					"are mutually exclusive; the apply's binding is the captured ab:<workflow>")
			}
			return runBootstrap(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), mode, bootstrapSpec{
				playbook:        args[0],
				workspace:       args[1],
				inventory:       inventory,
				vaultProject:    vaultProject,
				runSecrets:      runSecrets,
				captures:        captures,
				sourceName:      sourceName,
				requireApproval: requireApproval,
				timeout:         timeout,
				wait:            wait,
			})
		},
	}
	run.Flags().StringVar(&inventory, "inventory", "",
		"inventory path or registered name for the bootstrap play (empty: ansible discovers its own)")
	run.Flags().StringVar(&vaultProject, "vault-project", "",
		"derive the ansible-vault password for the bootstrap play from the envelope")
	run.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as provider/TF_VAR_ env for the apply step")
	run.Flags().StringArrayVar(&captureSpecs, "capture", nil,
		"seal a play-fetched file as env for the apply: NAME=FILE, repeatable "+
			"(NAME becomes the env var, FILE is fetched under the run's capture dir)")
	run.Flags().StringArrayVar(&captureFileSpecs, "capture-file", nil,
		"like --capture but deliver the sealed value as a FILE: NAME=FILE, repeatable "+
			"(NAME=<path under /dev/shm> for a tool that wants a KUBECONFIG file)")
	run.Flags().StringVar(&sourceName, "source", "",
		"wrap the chain in this registered source's .exocomp/hooks.yml lifecycle")
	run.Flags().BoolVar(&requireApproval, "require-approval", false,
		"gate the destructive apply behind an operator approval the daemon honors "+
			"mid-chain: the apply step waits in awaiting_approval until `exocomp approval` settles it")
	run.Flags().DurationVar(&timeout, "timeout", 0, "per-step timeout (0: no limit)")
	run.Flags().BoolVar(&wait, "wait", false, "follow the workflow until it reaches a terminal state")
	return run
}

func newBootstrapStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <workflow-id>",
		Short: "Print a bootstrap workflow's steps from the ledger",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()
			return printWorkflow(cmd.Context(), st, cmd.OutOrStdout(), args[0])
		},
	}
}

type bootstrapSpec struct {
	playbook        string
	workspace       string
	inventory       string
	vaultProject    string
	runSecrets      string
	captures        []executor.AnsibleCapture
	sourceName      string
	requireApproval bool
	timeout         time.Duration
	wait            bool
}

// parseCaptures turns each NAME=FILE into a declared capture target. NAME is the
// output name the sealed row and its env binding derive from; FILE is the path,
// relative to the run's capture dir, the play fetched the value into. A FILE that
// is absolute or climbs out of the capture dir is refused: a declared target must
// stay inside the private dir the driver reads back in-process. asFile marks the
// consumer delivery — a file path rather than an env value — and is the only
// difference between --capture and --capture-file.
func parseCaptures(specs []string, asFile bool) ([]executor.AnsibleCapture, error) {
	flag := "--capture"
	if asFile {
		flag = "--capture-file"
	}
	var out []executor.AnsibleCapture
	for _, s := range specs {
		name, file, ok := strings.Cut(s, "=")
		if !ok || name == "" || file == "" {
			return nil, fmt.Errorf("bootstrap: %s %q: want NAME=FILE", flag, s)
		}
		if filepath.IsAbs(file) || strings.HasPrefix(file, "..") || strings.Contains(file, "/../") {
			return nil, fmt.Errorf("bootstrap: %s %q: FILE must be a relative path inside the capture dir", flag, s)
		}
		out = append(out, executor.AnsibleCapture{Name: name, File: file, AsFile: asFile})
	}
	return out, nil
}

// bootstrapCaptureTargets reads a bootstrap play's declared captures and its
// workflow id off the job's first-class Captures field, in the executor shape the
// driver's sealer consumes. A job with no captures (every non-bootstrap play)
// returns nil and an empty workflow. The play's own extra-vars are a separate
// field now, so nothing has to be stripped: the play never sees this wiring.
func bootstrapCaptureTargets(jc *store.JobCaptures) ([]executor.AnsibleCapture, string) {
	if jc == nil {
		return nil, ""
	}
	captures := make([]executor.AnsibleCapture, len(jc.Targets))
	for i, t := range jc.Targets {
		captures[i] = executor.AnsibleCapture{Name: t.Name, File: t.File, AsFile: t.AsFile}
	}
	return captures, jc.Workflow
}

// bootstrapWorkflowView is the `--output json` projection of a started chain: the
// workflow id and its ordered step kinds, so a caller can confirm the chain is
// ansible-first without parsing prose.
type bootstrapWorkflowView struct {
	WorkflowID string   `json:"workflow_id"`
	Workspace  string   `json:"workspace"`
	Playbook   string   `json:"playbook"`
	Steps      []string `json:"steps"`
}

// composeBootstrapSteps builds the ansible-first chain: the bootstrap play, then
// the workspace's tofu apply. It is the handoff's [tofu-apply, ansible-play] with
// the order reversed, so the Sequencer runs the play first and the apply only if
// the play succeeded. The play carries no inventory in its argv when one is
// named, so the driver copies a registered inventory into the run's private dir
// the same way every other play does.
//
// Without --capture the apply binds --run-secrets and reads its own pre-existing
// credential (the Phase-0 shape). With --capture the play's declared outputs ride
// the step's first-class Captures field (the daemon's driver seals them under
// ab:<workflow>), and the apply binds that same ab:<workflow>, so the captured
// value reaches the kubernetes/helm provider as env with nothing for an operator
// to copy. The two credential sources are mutually exclusive (one binding slot),
// enforced at the CLI.
//
// Both steps carry the same Captures value: the play seals every target, the
// apply reads its --capture-file (AsFile) targets to know which bound values to
// stage as files. A field, not a reserved extra-var, is what a step re-armed
// after a crash keeps — rearmStep copies JobArgs.Captures.
func composeBootstrapSteps(workflowID, playbook, invPath, invName, tofuDir, workspace string, bs bootstrapSpec) []queue.Step {
	var captures *store.JobCaptures
	tofuBinding := bs.runSecrets
	if len(bs.captures) > 0 {
		captures = bootstrapCaptureArgs(workflowID, bs.captures)
		tofuBinding = executor.AnsibleOutputBinding(workflowID)
	}
	return []queue.Step{
		{
			Kind: "ansible-play",
			Spec: queue.Spec{
				Runbook:      "playbook",
				Kind:         executor.KindAnsible,
				Trigger:      bootstrapTrigger,
				Argv:         executor.AnsibleArgv(playbook, invPath),
				VaultProject: bs.vaultProject,
				Inventory:    invName,
				Captures:     captures,
				Timeout:      bs.timeout,
			},
		},
		{
			Kind: "tofu-apply",
			// The destructive apply is the gated step: with --require-approval the
			// Sequencer parks it in awaiting_approval until an operator decision
			// settles it, so a running daemon cannot apply the play's work unbidden.
			RequireApproval: bs.requireApproval,
			Spec: queue.Spec{
				Runbook:          workspaceRunbookPrefix + workspace,
				Kind:             executor.KindTofu,
				Trigger:          bootstrapTrigger,
				Argv:             executor.TofuArgv("apply", "-auto-approve"),
				Dir:              tofuDir,
				RunSecretBinding: tofuBinding,
				Captures:         captures,
				ConcurrencyKey:   "tofu:" + workspace,
				Timeout:          bs.timeout,
			},
		},
	}
}

// bootstrapCaptureArgs builds the chain's capture wiring as the first-class
// JobArgs field both steps carry: the workflow id its sealer binds under and the
// declared targets in the store shape. nil when the chain declared no captures.
func bootstrapCaptureArgs(workflowID string, captures []executor.AnsibleCapture) *store.JobCaptures {
	if len(captures) == 0 {
		return nil
	}
	targets := make([]store.JobCapture, len(captures))
	for i, c := range captures {
		targets[i] = store.JobCapture{Name: c.Name, File: c.File, AsFile: c.AsFile}
	}
	return &store.JobCaptures{Workflow: workflowID, Targets: targets}
}

// bootstrapStagedFileEnvs derives the env names a bootstrap apply must receive
// as /dev/shm-staged files rather than values: the --capture-file (AsFile)
// targets, folded to their env names, read off the tofu step's Captures
// field. nil for an env-only chain and for every non-bootstrap apply. An env
// name a capture folds to a non-POSIX identifier is dropped here for the same
// reason the sealer refuses to bind it.
func bootstrapStagedFileEnvs(jc *store.JobCaptures) []string {
	if jc == nil {
		return nil
	}
	var envs []string
	for _, t := range jc.Targets {
		if !t.AsFile {
			continue
		}
		if env, ok := executor.AnsibleOutputEnvName(t.Name); ok {
			envs = append(envs, env)
		}
	}
	return envs
}

// runBootstrap defines the play->apply chain as a ledger workflow and starts it.
// Like the handoff it only records the workflow: the daemon's sequencer releases
// step 1 (the play), and step 2 (the apply) only once step 1 succeeded, and it is
// the daemon that resumes the chain from the ledger after a restart.
func runBootstrap(ctx context.Context, out, warn io.Writer, mode string, bs bootstrapSpec) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	playbook, err := filepath.Abs(bs.playbook)
	if err != nil {
		return fmt.Errorf("bootstrap: resolve %s: %w", bs.playbook, err)
	}

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("bootstrap: resolve data dir: %w", err)
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

	ws, err := st.WorkspaceByName(ctx, bs.workspace)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("bootstrap: no workspace %q", bs.workspace)
	}
	if err != nil {
		return err
	}

	// The chain progresses only while a daemon drives the sequencer: it releases
	// each gated step, its workers run the job, and it resumes the workflow from
	// the ledger after a restart. Without one the workflow would sit gated.
	if !daemonRunning(ctx, dir) {
		return errors.New("bootstrap: no running daemon; start `exocomp serve` " +
			"(its sequencer drives the chain and resumes it after a restart)")
	}

	invPath, invName, err := resolveInventoryArg(ctx, st, dir, bs.inventory)
	if err != nil {
		return err
	}

	// The workflow id is fixed before the steps are composed: the play step's
	// sealer and the apply step's ab:<workflow> binding both derive from it, so
	// producer and consumer agree on the binding name without a round-trip.
	workflowID := fmt.Sprintf("bootstrap:%s:%d", ws.Name, time.Now().UTC().UnixNano())
	steps := composeBootstrapSteps(workflowID, playbook, invPath, invName, ws.TofuDir, ws.Name, bs)

	// A named source's lifecycle wraps the play->apply chain exactly as it wraps
	// the handoff: pre_run hooks before the play, post_run hooks and the health
	// gate after the apply. It reuses the handoff's wrapWithHooks so every
	// composer builds a hook step identically; a source with no hooks leaves the
	// two-step chain unchanged.
	if bs.sourceName != "" {
		src, serr := st.SourceByName(ctx, bs.sourceName)
		if errors.Is(serr, store.ErrNotFound) {
			return fmt.Errorf("bootstrap: no source %q", bs.sourceName)
		}
		if serr != nil {
			return serr
		}
		loc, serr := source.Resolve(dir, src.Kind, src.Name, src.URI, src.Writable)
		if serr != nil {
			return fmt.Errorf("bootstrap: %w", serr)
		}
		hk, serr := hooks.Resolve(loc.Root, hooksOverridePath(dir, src.Name))
		if serr != nil {
			return serr
		}
		if steps, serr = wrapWithHooks(loc.Root, loc.ReadOnly, hk, steps, bs.timeout); serr != nil {
			return fmt.Errorf("bootstrap: %w", serr)
		}
	}

	seq := queue.NewSequencer(queue.New(st, enqueueOnly{}, queue.Options{Workers: 1}))
	if err := seq.Start(ctx, workflowID, steps); err != nil {
		return err
	}

	if mode == outputJSON {
		return wire.WriteSuccess(out, bootstrapWorkflowKind, bootstrapWorkflowView{
			WorkflowID: workflowID,
			Workspace:  ws.Name,
			Playbook:   playbook,
			Steps:      stepKinds(steps),
		})
	}

	_, _ = fmt.Fprintf(out, "workflow %s started: play %s -> apply %s (%d step(s))\n",
		workflowID, playbook, ws.Name, len(steps))
	if !bs.wait {
		_, _ = fmt.Fprintf(out, "follow it with `exocomp bootstrap status %s`\n", workflowID)
		return nil
	}
	return followWorkflow(ctx, st, out, dir, workflowID)
}

// stepKinds projects a composed step list to its ordered ledger kinds, the shape
// the JSON envelope carries so ansible-first is machine-checkable.
func stepKinds(steps []queue.Step) []string {
	kinds := make([]string, len(steps))
	for i, s := range steps {
		kinds[i] = s.Kind
	}
	return kinds
}
