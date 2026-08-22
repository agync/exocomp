package main

import (
	"context"
	_ "embed"
	"encoding/json"
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
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newProvisionCmd)
}

// waitSSHRunbook is the seeded reachability gate the chain runs between the
// apply and the onboarding: an apply returns before the machine has booted.
//
//go:embed runbooks/wait-ssh.yml
var waitSSHRunbook []byte

// provisionOnboardPrefix marks the chain's onboarding job. The runbook field
// carries the workspace so the driver that claims the job can record the
// derived hosts' keys before the play pushes anything to them — the same shape
// `workspace:<name>` uses to bind a tofu job to its workspace.
const provisionOnboardPrefix = "provision-onboard:"

// provisionTrigger labels every job the chain enqueues.
const provisionTrigger = "provision"

// defaultSSHWait bounds the reachability step. Long enough for a cloud image to
// boot, short enough that a host that will never come up fails the chain
// instead of holding a queue slot forever.
const defaultSSHWait = 5 * time.Minute

func newProvisionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Provision, onboard, and converge a workspace's hosts as one chain",
		Long: "Chains `tofu apply` to the hosts the apply's declared outputs name: wait " +
			"for their SSH to come up, run the seeded onboarding runbook against them " +
			"(record the host key, push the fleet key, create the automation user), then " +
			"run the configure play against the same materialized inventory. It is one " +
			"step-ledger workflow (ADR 0008): step N+1 runs only if step N succeeded, and " +
			"a daemon that dies mid-chain resumes it from the ledger.",
	}
	cmd.AddCommand(newProvisionRunCmd(), newProvisionStatusCmd())
	return cmd
}

func newProvisionRunCmd() *cobra.Command {
	var (
		vaultProject string
		runSecrets   string
		onboardPath  string
		sshWait      time.Duration
		timeout      time.Duration
		wait         bool
	)
	run := &cobra.Command{
		Use:   "run <workspace> <playbook>",
		Short: "Apply, wait for SSH, onboard, and converge in one resumable chain",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvision(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), provisionSpec{
				workspace:    args[0],
				playbook:     args[1],
				vaultProject: vaultProject,
				runSecrets:   runSecrets,
				onboardPath:  onboardPath,
				sshWait:      sshWait,
				timeout:      timeout,
				wait:         wait,
			})
		},
	}
	run.Flags().StringVar(&vaultProject, "vault-project", "",
		"derive the ansible-vault password for the onboarding and configure plays")
	run.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as provider/TF_VAR_ env for the apply step")
	run.Flags().StringVar(&onboardPath, "onboard-playbook", "",
		"run this playbook as the onboarding step instead of the seeded runbook")
	run.Flags().DurationVar(&sshWait, "ssh-wait", defaultSSHWait,
		"how long the reachability step waits for each host's SSH port")
	run.Flags().DurationVar(&timeout, "timeout", 0, "per-step timeout (0: no limit)")
	run.Flags().BoolVar(&wait, "wait", false, "follow the workflow until it reaches a terminal state")
	return run
}

func newProvisionStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <workflow-id>",
		Short: "Print a provision workflow's steps from the ledger",
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

type provisionSpec struct {
	workspace    string
	playbook     string
	vaultProject string
	runSecrets   string
	onboardPath  string
	sshWait      time.Duration
	timeout      time.Duration
	wait         bool
}

// runProvision defines the four-step chain and starts it. Like the handoff it
// only records the workflow: the daemon's sequencer releases each step, its
// workers run the jobs, and it is the daemon that resumes the chain from the
// ledger after a crash.
//
// The hosts are not known when the workflow is defined — they are whatever the
// apply's declared outputs name — so the three ansible steps target the
// workspace's standing `tf:<workspace>` inventory by name and read whatever the
// apply materialized. That indirection is what lets one durable workflow span
// "no host exists yet" to "the host is converged".
func runProvision(ctx context.Context, out, warn io.Writer, ps provisionSpec) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	playbook, err := filepath.Abs(ps.playbook)
	if err != nil {
		return fmt.Errorf("provision: resolve %s: %w", ps.playbook, err)
	}

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("provision: resolve data dir: %w", err)
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

	ws, err := st.WorkspaceByName(ctx, ps.workspace)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("provision: no workspace %q", ps.workspace)
	}
	if err != nil {
		return err
	}

	if !daemonRunning(ctx, dir) {
		return errors.New("provision: no running daemon; start `exocomp serve` " +
			"(its sequencer drives the chain and resumes it after a restart)")
	}

	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("provision: open envelope (run `exocomp init` first): %w", err)
	}
	defer func() { _ = env.Close() }()

	// The fleet key must exist before the workflow is defined: the onboarding
	// step carries its PUBLIC half as an extra-var, and a step's argv is written
	// when the ledger row is created, not when the step runs.
	pub, err := ensureFleetKey(ctx, st, env)
	if err != nil {
		return err
	}

	waitPath, err := seedRunbook(dir, "wait-ssh.yml", waitSSHRunbook)
	if err != nil {
		return err
	}
	onboard := ps.onboardPath
	if onboard == "" {
		if onboard, err = seedOnboardRunbook(dir); err != nil {
			return err
		}
	} else if onboard, err = filepath.Abs(onboard); err != nil {
		return fmt.Errorf("provision: resolve %s: %w", ps.onboardPath, err)
	}

	// Publish the standing inventory name up front, pointing at the file the
	// apply will refresh, so the three later steps resolve it and so a schedule
	// or a reactive rule can already name `tf:<workspace>`.
	invName := handoffInventoryName(ws.Name)
	if _, err := st.MaterializeInventory(ctx, invName, handoffInventoryPath(dir, ws.Name)); err != nil {
		return err
	}

	if ps.sshWait <= 0 {
		ps.sshWait = defaultSSHWait
	}
	waitVars, err := extraVarsArgs(map[string]any{
		"exocomp_ssh_wait_timeout": int(ps.sshWait.Seconds()),
	})
	if err != nil {
		return err
	}
	onboardVars, err := extraVarsArgs(map[string]any{"exocomp_fleet_pubkey": pub})
	if err != nil {
		return err
	}

	steps := []queue.Step{
		{
			Kind: "tofu-apply",
			Spec: queue.Spec{
				Runbook:          workspaceRunbookPrefix + ws.Name,
				Kind:             executor.KindTofu,
				Trigger:          provisionTrigger,
				Argv:             executor.TofuArgv("apply", "-auto-approve"),
				Dir:              ws.TofuDir,
				RunSecretBinding: ps.runSecrets,
				ConcurrencyKey:   "tofu:" + ws.Name,
				Timeout:          ps.timeout,
			},
		},
		{
			Kind: "ssh-wait",
			Spec: queue.Spec{
				Runbook:   "provision-wait:" + ws.Name,
				Kind:      executor.KindAnsible,
				Trigger:   provisionTrigger,
				Argv:      executor.AnsibleArtifactArgv(waitPath, waitVars...),
				Inventory: invName,
				Timeout:   ps.timeout,
			},
		},
		{
			Kind: "host-onboard",
			Spec: queue.Spec{
				Runbook:      provisionOnboardPrefix + ws.Name,
				Kind:         executor.KindAnsible,
				Trigger:      provisionTrigger,
				Argv:         executor.AnsibleArtifactArgv(onboard, onboardVars...),
				VaultProject: ps.vaultProject,
				Inventory:    invName,
				Timeout:      ps.timeout,
			},
		},
		{
			Kind: "ansible-play",
			Spec: queue.Spec{
				Runbook:      "playbook",
				Kind:         executor.KindAnsible,
				Trigger:      provisionTrigger,
				Argv:         executor.AnsibleArtifactArgv(playbook),
				VaultProject: ps.vaultProject,
				Inventory:    invName,
				Timeout:      ps.timeout,
			},
		},
	}

	workflowID := fmt.Sprintf("provision:%s:%d", ws.Name, time.Now().UTC().UnixNano())
	seq := queue.NewSequencer(queue.New(st, enqueueOnly{}, queue.Options{Workers: 1}))
	if err := seq.Start(ctx, workflowID, steps); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "workflow %s started: apply %s -> wait -> onboard -> play %s (%d steps)\n",
		workflowID, ws.Name, playbook, len(steps))
	_, _ = fmt.Fprintf(out, "the onboarding step authorizes this fleet key on each derived host:\n%s\n", pub)

	if !ps.wait {
		_, _ = fmt.Fprintf(out, "follow it with `exocomp provision status %s`\n", workflowID)
		return nil
	}
	return followWorkflow(ctx, st, out, dir, workflowID)
}

// extraVarsArgs renders extra-vars as a single JSON `-e` argument. The k=v form
// ansible also accepts splits on whitespace, so a value with a space in it — an
// authorized_keys line, which is exactly what the onboarding step passes —
// silently becomes several broken variables. JSON has no such ambiguity.
func extraVarsArgs(vars map[string]any) ([]string, error) {
	blob, err := json.Marshal(vars)
	if err != nil {
		return nil, fmt.Errorf("provision: encode extra vars: %w", err)
	}
	return []string{"-e", string(blob)}, nil
}

// seedRunbook writes one embedded runbook into the data volume so it runs
// through the queue like any other play, and returns its path. It is rewritten
// on every call so an upgrade's newer runbook lands.
func seedRunbook(dir, name string, body []byte) (string, error) {
	runbookDir := filepath.Join(dir, "runbooks")
	if err := os.MkdirAll(runbookDir, 0o700); err != nil {
		return "", fmt.Errorf("seed runbook: create runbook dir: %w", err)
	}
	path := filepath.Join(runbookDir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", fmt.Errorf("seed runbook: write %s: %w", name, err)
	}
	return path, nil
}

// recordProvisionedHostKeys pins the host keys of the hosts an onboarding step
// is about to touch, BEFORE it touches them. It runs in the driver that claimed
// the job because that is the first moment the hosts are known: they come from
// the inventory the apply materialized minutes earlier, not from the command
// that defined the workflow.
//
// Trust on first use is deliberate and one-way — a host whose recorded key has
// CHANGED fails the step (store.ErrKnownHostChanged) rather than being
// re-pinned, so a rekey is confirmed out of band with `host onboard --rotate`
// and an attacker at scan time cannot inherit the fleet key.
//
// A job that is not a chain onboarding step, and a host reached without SSH,
// are both no-ops.
func recordProvisionedHostKeys(ctx context.Context, st *store.Store, job store.Job,
	inventoryPath string, sink executor.EventSink) error {
	if !strings.HasPrefix(job.Runbook, provisionOnboardPrefix) || inventoryPath == "" {
		return nil
	}
	body, err := os.ReadFile(inventoryPath)
	if err != nil {
		return fmt.Errorf("provision: read inventory %s: %w", inventoryPath, err)
	}
	hosts, err := executor.InventoryHosts(body)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("provision: inventory %s names no hosts to onboard", inventoryPath)
	}

	for _, h := range hosts {
		if h.Local {
			continue
		}
		scanned, err := keyscanHost(ctx, h.Address, h.Port)
		if err != nil {
			return fmt.Errorf("provision: %w", err)
		}
		n, err := recordScanned(ctx, st, scanned, false, io.Discard)
		if err != nil {
			return fmt.Errorf("provision: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("provision: no host keys returned for %s", h.Name)
		}
		sink.Emit(executor.Event{
			Ts:    time.Now().UTC(),
			Level: executor.LevelSystem,
			Msg:   fmt.Sprintf("pinned %d host key(s) for %s before onboarding it", n, h.Name),
		})
	}
	return nil
}
