package service

import (
	"context"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// PlaybookInput is a resolved ansible run. The caller has already turned flags
// into these values: an absolute playbook path, the inventory split into the
// path ansible targets and the registered name whose bindings a daemon
// re-resolves, and the validated ansible passthrough and extra-vars.
type PlaybookInput struct {
	PlaybookPath        string
	InventoryPath       string
	InventoryName       string
	VaultProject        string
	RunSecrets          string
	Passthrough         []string
	ExtraVars           map[string]string
	TolerateUnreachable bool
	IdempotencyKey      string
	Timeout             time.Duration
	// Dir is the run's working directory: a --source run's checkout root, so
	// ansible loads the tree's own ./ansible.cfg and its relative inventory,
	// roles, and collections. Empty leaves the working directory to the executor
	// (the sandboxed tier's per-job scratch dir), which is the raw-path form's
	// behavior. DirReadOnly carries the source's read-only posture: a read-only
	// checkout is granted read, never write, so the run reads the tree but cannot
	// write back to it (the sandbox enforces this in executor.Sandbox.Run).
	Dir         string
	DirReadOnly bool
	// RunnerName names a registered runner whose memory_max/cpu_quota the
	// sandboxed execution path should enforce as a per-job cgroup cap. Empty
	// is the default, uncapped path.
	RunnerName string
	// Source names the registered source this run executes from, carried onto
	// the job row for the "running" event and every job face to display.
	// Empty on a raw-path run.
	Source string
	// Privileged asks that the run execute UNSANDBOXED (the disabled tier). It
	// is operator-gated at the calling face; the verb only carries it onto the
	// spec and records the escalation. Off by default is normal tiering.
	Privileged bool
	// EgressPorts are extra outbound TCP ports this run may open on top of the
	// ansible kind's built-in SSH port, for an inventory host on a nonstandard
	// port. Empty is the default posture: no declaration, no widening.
	EgressPorts []int
	// Actor identifies a non-CLI caller (an API token) for the job's event
	// ledger; the zero value records nothing (see recordActor).
	Actor Actor
}

// RunResult carries the job a verb enqueued. A face reads its id and, when the
// Enqueuer ran it inline, its terminal state and exit code.
type RunResult struct {
	Job store.Job
}

// RunPlaybook assembles the ansible job spec and enqueues it. The vault project
// and binding names travel on the row, never the material they resolve, so a
// daemon that claims the job derives the same secrets.
func (s *Service) RunPlaybook(ctx context.Context, enq Enqueuer, in PlaybookInput) (RunResult, error) {
	spec := queue.Spec{
		Runbook:             "playbook",
		Kind:                executor.KindAnsible,
		Trigger:             "manual",
		Argv:                executor.AnsibleArgv(in.PlaybookPath, in.InventoryPath, in.Passthrough...),
		Dir:                 in.Dir,
		DirReadOnly:         in.DirReadOnly,
		VaultProject:        in.VaultProject,
		RunSecretBinding:    in.RunSecrets,
		Inventory:           in.InventoryName,
		Source:              in.Source,
		ExtraVars:           in.ExtraVars,
		TolerateUnreachable: in.TolerateUnreachable,
		IdempotencyKey:      in.IdempotencyKey,
		Timeout:             in.Timeout,
		RunnerName:          in.RunnerName,
		Privileged:          in.Privileged,
		EgressPorts:         in.EgressPorts,
	}
	job, err := enq.Enqueue(ctx, spec)
	if err != nil {
		return RunResult{}, err
	}
	if err := s.recordActor(ctx, job.ID, in.Actor); err != nil {
		return RunResult{}, err
	}
	if in.Privileged {
		if err := s.recordPrivileged(ctx, job.ID, in.Actor); err != nil {
			return RunResult{}, err
		}
	}
	return RunResult{Job: job}, nil
}

// Reenqueue enqueues an already-assembled spec and records who asked — the same
// attribution tail RunPlaybook and RunAdhoc run. It is the primitive the web
// "Run again" resubmit is built on: the calling face reconstructs the spec by
// cloning a finished job's stored args and applying the modal's overrides, and
// this lands the actor and, for a privileged spec, the escalation on the job
// ledger. Enqueue-time gating (an escalation must pass the operator scope) is the
// caller's, exactly as it is for every other run verb here.
func (s *Service) Reenqueue(ctx context.Context, enq Enqueuer, spec queue.Spec, actor Actor) (RunResult, error) {
	job, err := enq.Enqueue(ctx, spec)
	if err != nil {
		return RunResult{}, err
	}
	if err := s.recordActor(ctx, job.ID, actor); err != nil {
		return RunResult{}, err
	}
	if spec.Privileged {
		if err := s.recordPrivileged(ctx, job.ID, actor); err != nil {
			return RunResult{}, err
		}
	}
	return RunResult{Job: job}, nil
}
