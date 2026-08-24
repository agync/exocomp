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
	// RunnerName names a registered runner whose memory_max/cpu_quota the
	// sandboxed execution path should enforce as a per-job cgroup cap. Empty
	// is the default, uncapped path.
	RunnerName string
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
		VaultProject:        in.VaultProject,
		RunSecretBinding:    in.RunSecrets,
		Inventory:           in.InventoryName,
		ExtraVars:           in.ExtraVars,
		TolerateUnreachable: in.TolerateUnreachable,
		IdempotencyKey:      in.IdempotencyKey,
		Timeout:             in.Timeout,
		RunnerName:          in.RunnerName,
	}
	job, err := enq.Enqueue(ctx, spec)
	if err != nil {
		return RunResult{}, err
	}
	if err := s.recordActor(ctx, job.ID, in.Actor); err != nil {
		return RunResult{}, err
	}
	return RunResult{Job: job}, nil
}
