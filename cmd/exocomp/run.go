package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/verify"
)

func init() {
	registerCommand(newRunCmd)
}

// hooksOverrideDir is where an operator-registered hooks.yml override lives,
// keyed by source name, inside the data dir. It wins over the checkout's own
// .exocomp/hooks.yml — the same override-wins precedence the source model uses.
const hooksOverrideDir = "hooks"

// hookShell is the interpreter a hook step runs under. Both an inline `run`
// command and a `script` path go through it, so the hook contract is one thing
// (a shell step) and a hook that fails is a non-zero exit the sequencer blocks
// the run on.
const hookShell = "/bin/sh"

// hooksOverridePath is the operator override file for a source's hooks.
func hooksOverridePath(dataDir, sourceName string) string {
	return filepath.Join(dataDir, hooksOverrideDir, sourceName+".yml")
}

func newRunCmd() *cobra.Command {
	var (
		inventory    string
		vaultProject string
		runSecrets   string
		runnerName   string
		timeout      time.Duration
		wait         bool
		privileged   bool
		egress       []string
		sshIdentity  []string
	)
	cmd := &cobra.Command{
		Use:   "run <source> <playbook>",
		Short: "Run a source's playbook as a lifecycle workflow (pre-hooks -> play -> post-hooks -> gate)",
		Long: "Composes a source's run into one step-ledger workflow: its .exocomp/hooks.yml " +
			"pre_run steps, then the play, then the post_run steps, then the health/verification " +
			"gate. Each step is a queue job; a pre-hook that fails leaves the play blocked, and a " +
			"gate probe that fails marks the whole run failed even when the play exited 0. The " +
			"chain is driven by a running `exocomp serve`; the playbook path is relative to the " +
			"source checkout.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			egressPorts, err := parseEgressPorts(egress)
			if err != nil {
				return err
			}
			identityFiles, err := parseSSHIdentityFiles(sshIdentity)
			if err != nil {
				return err
			}
			if privileged {
				warnPrivileged(cmd.ErrOrStderr())
			}
			return runWithHooks(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), runSpec{
				sourceName:       args[0],
				playbook:         args[1],
				inventory:        inventory,
				vaultProject:     vaultProject,
				runSecrets:       runSecrets,
				runnerName:       runnerName,
				timeout:          timeout,
				wait:             wait,
				privileged:       privileged,
				egressPorts:      egressPorts,
				sshIdentityFiles: identityFiles,
			})
		},
	}
	cmd.Flags().StringVar(&inventory, "inventory", "",
		"inventory path or registered inventory name (empty: ansible discovers its own)")
	cmd.Flags().StringVar(&vaultProject, "vault-project", "",
		"derive the ansible-vault password for the play from the envelope")
	cmd.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as named env for the play")
	cmd.Flags().StringVar(&runnerName, "runner", "",
		"enforce this registered runner's memory_max/cpu_quota as a per-job cgroup cap "+
			"(local-sandboxed only; refused, never run uncapped, if it cannot be enforced)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "per-step timeout (0: no limit)")
	cmd.Flags().BoolVar(&wait, "wait", false, "follow the workflow until it reaches a terminal state")
	cmd.Flags().BoolVar(&privileged, "privileged", false,
		"run the play step UNSANDBOXED — full host access, no kernel sandbox; operator only (see docs/security/threat-model.md)")
	cmd.Flags().StringSliceVar(&egress, "egress", nil,
		"grant the play step outbound TCP on <port> (repeatable, or comma-separated) on top of the sandbox's SSH port 22")
	cmd.Flags().StringSliceVar(&sshIdentity, "ssh-identity", nil,
		"grant the play step read access to an ssh identity key file (repeatable; each file is granted individually, never its directory)")

	cmd.AddCommand(newRunStatusCmd())
	return cmd
}

func newRunStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <workflow-id>",
		Short: "Print a run workflow's steps from the ledger",
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

type runSpec struct {
	sourceName   string
	playbook     string
	inventory    string
	vaultProject string
	runSecrets   string
	runnerName   string
	timeout      time.Duration
	wait         bool
	// passthrough and extraVars carry `playbook run`'s ansible flags into the
	// play step of a hooked --source workflow, so --limit/--tags/-e/--check work
	// the same whether or not the source declares hooks.
	passthrough []string
	extraVars   map[string]string
	// tolerateUnreachable opts the play step into the degraded outcome.
	tolerateUnreachable bool
	// privileged runs the play step unsandboxed (the disabled tier). Only the
	// play carries it; the hook and gate steps keep the daemon's tier.
	privileged bool
	// egressPorts are the extra outbound TCP ports the play step's sandboxed
	// run may open on top of the kind's SSH port 22, validated at the flag
	// parse.
	egressPorts []int
	// sshIdentityFiles are single ssh identity key files the play step may
	// read, validated and absolutized at the flag parse.
	sshIdentityFiles []string
}

// runWithHooks resolves a source's hooks + gate and composes them with the main
// play into one gated ledger workflow, then starts it. Like the handoff, the
// daemon's sequencer is the sole executor: it releases each step and runs the
// next only once the prior one succeeded, so a failed pre-hook blocks the play
// and a failed gate fails the run.
func runWithHooks(ctx context.Context, out, warn io.Writer, rs runSpec) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("run: resolve data dir: %w", err)
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

	src, err := st.SourceByName(ctx, rs.sourceName)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("run: no source %q", rs.sourceName)
	}
	if err != nil {
		return err
	}

	loc, err := source.Resolve(dir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	hk, err := hooks.Resolve(loc.Root, hooksOverridePath(dir, src.Name))
	if err != nil {
		return err
	}

	playbook := rs.playbook
	if !filepath.IsAbs(playbook) {
		playbook = filepath.Join(loc.Root, playbook)
	}

	invPath, invName, err := resolveInventoryArg(ctx, st, dir, rs.inventory)
	if err != nil {
		return err
	}

	steps, err := composeRunSteps(loc.Root, loc.ReadOnly, playbook, invPath, invName, rs, hk)
	if err != nil {
		return err
	}

	// The chain progresses only while a daemon drives the sequencer; without one
	// the gated steps would sit unreleased. This is the same contract as the
	// handoff, and it is why the hook and gate drivers live in the daemon.
	if !daemonRunning(ctx, dir) {
		return errors.New("run: no running daemon; start `exocomp serve` " +
			"(its sequencer drives the workflow and resumes it after a restart)")
	}

	workflowID := fmt.Sprintf("run:%s:%d", src.Name, time.Now().UTC().UnixNano())
	seq := queue.NewSequencer(queue.New(st, enqueueOnly{}, queue.Options{Workers: 1}))
	if err := seq.Start(ctx, workflowID, steps); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "workflow %s started: %d step(s) over source %s\n",
		workflowID, len(steps), src.Name)
	summariseRun(out, hk)

	if !rs.wait {
		_, _ = fmt.Fprintf(out, "follow it with `exocomp run status %s`\n", workflowID)
		return nil
	}
	return followWorkflow(ctx, st, out, dir, workflowID)
}

// composeRunSteps builds the ordered step list: pre_run hooks, the play, then
// post_run hooks, then the gate. Each hook is a sandboxed shell step; the play
// is an ordinary ansible job; the gate is one verify step whose success is the
// declared probes, not an exit code.
func composeRunSteps(root string, readOnly bool, playbook, invPath, invName string, rs runSpec, hk hooks.Hooks) ([]queue.Step, error) {
	var steps []queue.Step
	for _, h := range hk.PreRun {
		steps = append(steps, hookStep(root, readOnly, h, rs.sourceName, rs.timeout))
	}

	steps = append(steps, queue.Step{
		Kind: executor.KindAnsible,
		Spec: queue.Spec{
			Runbook:             "playbook",
			Kind:                executor.KindAnsible,
			Trigger:             "run",
			Argv:                executor.AnsibleArgv(playbook, invPath, rs.passthrough...),
			Dir:                 root,
			DirReadOnly:         readOnly,
			VaultProject:        rs.vaultProject,
			RunSecretBinding:    rs.runSecrets,
			Inventory:           invName,
			Source:              rs.sourceName,
			ExtraVars:           rs.extraVars,
			TolerateUnreachable: rs.tolerateUnreachable,
			RunnerName:          rs.runnerName,
			Privileged:          rs.privileged,
			EgressPorts:         rs.egressPorts,
			SSHIdentityFiles:    rs.sshIdentityFiles,
			Timeout:             rs.timeout,
		},
	})

	for _, h := range hk.PostRun {
		steps = append(steps, hookStep(root, readOnly, h, rs.sourceName, rs.timeout))
	}

	if len(hk.Gate) > 0 {
		argv, err := verify.Encode(hk.Gate)
		if err != nil {
			return nil, fmt.Errorf("run: gate: %w", err)
		}
		steps = append(steps, queue.Step{
			Kind: verify.JobKind,
			Spec: queue.Spec{
				Runbook: "gate",
				Kind:    verify.JobKind,
				Trigger: "run",
				Argv:    argv,
				Source:  rs.sourceName,
				Timeout: rs.timeout,
			},
		})
	}
	return steps, nil
}

// hookStep builds one hook's ledger step. Both an inline command and a script
// path run under the shell, with the checkout as the working directory and the
// hook's optional run_secret binding carried so the daemon resolves it to named
// env at execution time.
func hookStep(root string, readOnly bool, h hooks.Hook, sourceName string, timeout time.Duration) queue.Step {
	argv := []string{hookShell, "-c", h.Run}
	if h.Script != "" {
		argv = []string{hookShell, filepath.Join(root, h.Script)}
	}
	return queue.Step{
		Kind: hooks.JobKind,
		Spec: queue.Spec{
			Runbook:          "hook:" + h.Name,
			Kind:             hooks.JobKind,
			Trigger:          "run",
			Argv:             argv,
			Dir:              root,
			DirReadOnly:      readOnly,
			Source:           sourceName,
			RunSecretBinding: h.Secret,
			Timeout:          timeout,
		},
	}
}

// summariseRun tells the operator what the composed workflow will do, so the
// step ordering is visible before the ledger fills in.
func summariseRun(out io.Writer, hk hooks.Hooks) {
	for _, h := range hk.PreRun {
		_, _ = fmt.Fprintf(out, "  pre_run  %s\n", h.Name)
	}
	_, _ = fmt.Fprintln(out, "  play     <main>")
	for _, h := range hk.PostRun {
		_, _ = fmt.Fprintf(out, "  post_run %s\n", h.Name)
	}
	for _, p := range hk.Gate {
		_, _ = fmt.Fprintf(out, "  gate     %s (%s)\n", p.Name, probeKind(p))
	}
}

// probeKind names a probe's single check, for the summary and the lister.
func probeKind(p verify.Probe) string {
	switch {
	case p.HTTP != nil:
		return string(verify.KindHTTP)
	case p.DNS != nil:
		return string(verify.KindDNS)
	case p.TLS != nil:
		return string(verify.KindTLS)
	case p.Metric != nil:
		return string(verify.KindMetric)
	default:
		return "?"
	}
}
