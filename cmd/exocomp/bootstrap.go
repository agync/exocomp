package main

import (
	"context"
	"encoding/json"
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

// bootstrapCaptureVar carries the operator's --capture declarations from the CLI
// to the daemon's ansible driver on the play step's job. store.JobArgs has no
// capture field (the queue/store lane owns that shape), so the declarations ride
// the one durable per-job map that exists — ExtraVars — under a reserved key the
// driver strips before the play, keeping the play's own var namespace clean. The
// value is JSON: a []executor.AnsibleCapture. Because rearmStep does not copy
// ExtraVars, a play re-armed after a mid-run crash loses its captures; a
// JobArgs.Captures field would fix that and is the follow-on this channel stands
// in for.
const bootstrapCaptureVar = "exocomp_bootstrap_captures"

// bootstrapWorkflowVar carries the chain's workflow id to the play step's sealer
// on the same channel, so the sealed rows and the ab:<workflow> binding derive
// from the exact id the composer bound into the tofu step, with no coupling to
// the queue's internal step-key format.
const bootstrapWorkflowVar = "exocomp_bootstrap_workflow"

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
		inventory       string
		vaultProject    string
		runSecrets      string
		captureSpecs    []string
		sourceName      string
		requireApproval bool
		timeout         time.Duration
		wait            bool
	)
	run := &cobra.Command{
		Use:   "run <playbook> <workspace>",
		Short: "Define and start a bootstrap-play->apply workflow",
		Long: "Chains the bootstrap play to the workspace's apply as one step-ledger " +
			"workflow, play first. The play runs against --inventory (localhost by " +
			"default, since no tofu output exists yet to build one from); the apply runs " +
			"only if the play succeeded. With --source, the source's .exocomp/hooks.yml " +
			"wraps the chain exactly as it wraps `handoff run`. --require-approval gates " +
			"the destructive apply behind the confirmation contract.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			// The chain runs a destructive `tofu apply`. --require-approval makes
			// that gate explicit at the CLI: the operator pre-approves with
			// --assume-yes rather than the chain starting unprompted. A durable
			// per-step approval the daemon would honor mid-chain is not something
			// Phase 0 can enforce — the Sequencer releases a gated workflow step
			// on its own — so this confirmation is the Phase-0 guardrail.
			if requireApproval {
				if err := confirmDestructive(cmd, false,
					"bootstrap runs a destructive tofu apply", ""); err != nil {
					return err
				}
			}
			captures, err := parseCaptures(captureSpecs)
			if err != nil {
				return err
			}
			// The apply carries one run-secret binding. With --capture that slot is
			// the chain's own ab:<workflow>; a pre-existing --run-secrets would have
			// to share it, which one field cannot, so the two are refused together
			// rather than one silently winning.
			if len(captures) > 0 && runSecrets != "" {
				return errors.New("bootstrap: --capture and --run-secrets are mutually " +
					"exclusive; the apply's binding is the captured ab:<workflow>")
			}
			return runBootstrap(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), mode, bootstrapSpec{
				playbook:     args[0],
				workspace:    args[1],
				inventory:    inventory,
				vaultProject: vaultProject,
				runSecrets:   runSecrets,
				captures:     captures,
				sourceName:   sourceName,
				timeout:      timeout,
				wait:         wait,
			})
		},
	}
	run.Flags().StringVar(&inventory, "inventory", "",
		"inventory path or registered name for the bootstrap play (default: localhost)")
	run.Flags().StringVar(&vaultProject, "vault-project", "",
		"derive the ansible-vault password for the bootstrap play from the envelope")
	run.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as provider/TF_VAR_ env for the apply step")
	run.Flags().StringArrayVar(&captureSpecs, "capture", nil,
		"seal a play-fetched file as env for the apply: NAME=FILE, repeatable "+
			"(NAME becomes the env var, FILE is fetched under the run's capture dir)")
	run.Flags().StringVar(&sourceName, "source", "",
		"wrap the chain in this registered source's .exocomp/hooks.yml lifecycle")
	run.Flags().BoolVar(&requireApproval, "require-approval", false,
		"gate the destructive chain: require --assume-yes rather than start unprompted")
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
	playbook     string
	workspace    string
	inventory    string
	vaultProject string
	runSecrets   string
	captures     []executor.AnsibleCapture
	sourceName   string
	timeout      time.Duration
	wait         bool
}

// parseCaptures turns each --capture NAME=FILE into a declared capture target.
// NAME is the output name the sealed row and its env binding derive from; FILE is
// the path, relative to the run's capture dir, the play fetched the value into. A
// FILE that is absolute or climbs out of the capture dir is refused: a declared
// target must stay inside the private dir the driver reads back in-process.
func parseCaptures(specs []string) ([]executor.AnsibleCapture, error) {
	var out []executor.AnsibleCapture
	for _, s := range specs {
		name, file, ok := strings.Cut(s, "=")
		if !ok || name == "" || file == "" {
			return nil, fmt.Errorf("bootstrap: --capture %q: want NAME=FILE", s)
		}
		if filepath.IsAbs(file) || strings.HasPrefix(file, "..") || strings.Contains(file, "/../") {
			return nil, fmt.Errorf("bootstrap: --capture %q: FILE must be a relative path inside the capture dir", s)
		}
		out = append(out, executor.AnsibleCapture{Name: name, File: file})
	}
	return out, nil
}

// extractBootstrapCaptures pulls a bootstrap play's capture declarations and its
// workflow id off the job's extra-vars — the channel composeBootstrapSteps
// smuggled them through — and returns the extra-vars with those reserved keys
// removed, so the play never sees exocomp internal wiring in its own var
// namespace. A job with no reserved key (every non-bootstrap play) returns its
// extra-vars unchanged and no captures.
func extractBootstrapCaptures(extraVars map[string]string) ([]executor.AnsibleCapture, string, map[string]string) {
	raw, ok := extraVars[bootstrapCaptureVar]
	if !ok {
		return nil, "", extraVars
	}
	workflow := extraVars[bootstrapWorkflowVar]
	var captures []executor.AnsibleCapture
	// The composer wrote this JSON; a decode failure is a corrupted row, not
	// operator input, so it drops the captures rather than failing the play.
	_ = json.Unmarshal([]byte(raw), &captures)
	filtered := make(map[string]string, len(extraVars))
	for k, v := range extraVars {
		if k == bootstrapCaptureVar || k == bootstrapWorkflowVar {
			continue
		}
		filtered[k] = v
	}
	if len(filtered) == 0 {
		filtered = nil
	}
	return captures, workflow, filtered
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
// credential (the Phase-0 shape). With --capture the play's declared outputs are
// smuggled onto its step as reserved extra-vars (the daemon's driver seals them
// under ab:<workflow>), and the apply binds that same ab:<workflow>, so the
// captured value reaches the kubernetes/helm provider as env with nothing for an
// operator to copy. The two credential sources are mutually exclusive (one
// binding slot), enforced at the CLI.
func composeBootstrapSteps(workflowID, playbook, invPath, invName, tofuDir, workspace string, bs bootstrapSpec) []queue.Step {
	var playExtraVars map[string]string
	tofuBinding := bs.runSecrets
	if len(bs.captures) > 0 {
		playExtraVars = bootstrapCaptureVars(workflowID, bs.captures)
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
				ExtraVars:    playExtraVars,
				Timeout:      bs.timeout,
			},
		},
		{
			Kind: "tofu-apply",
			Spec: queue.Spec{
				Runbook:          workspaceRunbookPrefix + workspace,
				Kind:             executor.KindTofu,
				Trigger:          bootstrapTrigger,
				Argv:             executor.TofuArgv("apply", "-auto-approve"),
				Dir:              tofuDir,
				RunSecretBinding: tofuBinding,
				ConcurrencyKey:   "tofu:" + workspace,
				Timeout:          bs.timeout,
			},
		},
	}
}

// bootstrapCaptureVars encodes the play step's capture declarations and workflow
// id as the reserved extra-vars the daemon's driver reads back and strips. It is
// the only durable per-job carrier available without a JobArgs field (see the
// reserved-key consts).
func bootstrapCaptureVars(workflowID string, captures []executor.AnsibleCapture) map[string]string {
	blob, err := json.Marshal(captures)
	if err != nil {
		// Unreachable: AnsibleCapture is two strings.
		panic("bootstrap: encode captures: " + err.Error())
	}
	return map[string]string{
		bootstrapCaptureVar:  string(blob),
		bootstrapWorkflowVar: workflowID,
	}
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
		loc, serr := source.Resolve(dir, src.Kind, src.Name, src.URI)
		if serr != nil {
			return fmt.Errorf("bootstrap: %w", serr)
		}
		hk, serr := hooks.Resolve(loc.Root, hooksOverridePath(dir, src.Name))
		if serr != nil {
			return serr
		}
		if steps, serr = wrapWithHooks(loc.Root, hk, steps, bs.timeout); serr != nil {
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
