package service

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// PlanInput is a resolved tofu plan. ArtifactsDir is where the plan file is
// written; the verb names the file so a later apply --plan can find it.
type PlanInput struct {
	Workspace      string
	TofuDir        string
	ArtifactsDir   string
	RunSecrets     string
	IdempotencyKey string
	Timeout        time.Duration
	// Trigger records who asked (TriggerManual/TriggerAPI); empty defaults to
	// manual. The daemon reads it back to decide whether to seal the plan.
	Trigger string
}

// PlanResult carries the enqueued job and the path the plan file will be
// written to, so the caller can seal and record it once the run finishes.
type PlanResult struct {
	Job      store.Job
	PlanPath string
}

// PlanWorkspace assembles a tofu plan job and enqueues it. The plan is written
// with -out so an apply can consume it; the file name is unique per run.
func (s *Service) PlanWorkspace(ctx context.Context, enq Enqueuer, in PlanInput) (PlanResult, error) {
	planPath := filepath.Join(in.ArtifactsDir,
		fmt.Sprintf("%s-%d.tfplan", in.Workspace, s.now().UTC().UnixNano()))
	trigger := in.Trigger
	if trigger == "" {
		trigger = TriggerManual
	}
	spec := queue.Spec{
		Runbook:          WorkspaceRunbook(in.Workspace),
		Kind:             executor.KindTofu,
		Trigger:          trigger,
		Argv:             executor.TofuArgv("plan", "-out="+planPath),
		Dir:              in.TofuDir,
		RunSecretBinding: in.RunSecrets,
		ConcurrencyKey:   workspaceConcurrencyKey(in.Workspace),
		IdempotencyKey:   in.IdempotencyKey,
		Timeout:          in.Timeout,
	}
	job, err := enq.Enqueue(ctx, spec)
	if err != nil {
		return PlanResult{}, err
	}
	return PlanResult{Job: job, PlanPath: planPath}, nil
}

// PlanGate is a saved plan's recorded policy verdict, reduced to the two
// outcomes that route an apply: deny makes the plan unconsumable, and
// require-approval sends it through the gate. Policy names the rule that decided.
type PlanGate struct {
	Deny            bool
	RequireApproval bool
	Policy          string
}

// PlanDeniedError reports that a saved plan carries a deny verdict, so its apply
// is refused before anything is enqueued or decrypted.
type PlanDeniedError struct {
	PlanArtifactID int64
	Policy         string
}

func (e *PlanDeniedError) Error() string {
	return fmt.Sprintf("workspace apply: plan %d is unconsumable: denied by policy %q",
		e.PlanArtifactID, e.Policy)
}

// ApplyInput is a resolved tofu apply. SavedPlan is the plan being consumed
// (nil for a fresh apply, or an operator's own plan file that carries no
// verdict); Gate is its verdict. WorkspaceSerial is the serial the plan was made
// against, recorded on a gated approval for the stale-plan check.
type ApplyInput struct {
	Workspace       string
	WorkspaceSerial int64
	TofuDir         string
	RunSecrets      string
	IdempotencyKey  string
	Timeout         time.Duration
	SavedPlan       *store.Artifact
	Gate            PlanGate
}

// WorkspaceRun is a plain (non-gated) tofu run. Parent is the saved plan an
// apply consumed, for the caller to link to the new job; Release removes the
// plaintext plan copy the run read and must be called once the run is recorded.
type WorkspaceRun struct {
	Job     store.Job
	Parent  *store.Artifact
	Release func()
}

// GatedApproval is a require-approval apply that was routed to the gate instead
// of run: its job sits in awaiting_approval and this approval releases it.
type GatedApproval struct {
	ApprovalID     int64
	JobID          int64
	State          store.ApprovalState
	ExpiresAt      time.Time
	Policy         string
	PlanArtifactID int64
}

// ApplyResult is either a gated apply (Gated set, Approval populated, Run nil)
// or a plain run (Run populated).
type ApplyResult struct {
	Gated    bool
	Approval GatedApproval
	Run      *WorkspaceRun
}

// ApplyWorkspace enqueues a tofu apply, routing a saved plan through the
// approval gate when its verdict demands it. A deny verdict is refused outright;
// a require-approval verdict lands the job in awaiting_approval with a fresh
// approval and never opens the plan. Any other apply builds its argv through
// buildArgv — deferred so the plaintext plan is decrypted only for a run that
// will actually happen — and enqueues it plainly.
// The deriver mints the approval capability for a gated apply; the running
// envelope satisfies it. It is an interface so a gate can be exercised without a
// real keyring.
func (s *Service) ApplyWorkspace(ctx context.Context, enq Enqueuer, deriver reactive.Deriver,
	in ApplyInput,
	buildArgv func(context.Context) (argv []string, parent *store.Artifact, release func(), err error),
) (ApplyResult, error) {
	if in.SavedPlan != nil {
		if in.Gate.Deny {
			return ApplyResult{}, &PlanDeniedError{PlanArtifactID: in.SavedPlan.ID, Policy: in.Gate.Policy}
		}
		if in.Gate.RequireApproval {
			return s.gatedApply(ctx, enq, deriver, in)
		}
	}

	argv, parent, release, err := buildArgv(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	spec := queue.Spec{
		Runbook:          WorkspaceRunbook(in.Workspace),
		Kind:             executor.KindTofu,
		Trigger:          "manual",
		Argv:             argv,
		Dir:              in.TofuDir,
		RunSecretBinding: in.RunSecrets,
		ConcurrencyKey:   workspaceConcurrencyKey(in.Workspace),
		IdempotencyKey:   in.IdempotencyKey,
		Timeout:          in.Timeout,
	}
	job, err := enq.Enqueue(ctx, spec)
	if err != nil {
		// The run never happened; drop the plaintext plan the argv builder opened.
		if release != nil {
			release()
		}
		return ApplyResult{}, err
	}
	return ApplyResult{Run: &WorkspaceRun{Job: job, Parent: parent, Release: release}}, nil
}

// gatedApply routes a require-approval plan through the existing gate rather than
// running it: EnqueueGated lands the job in awaiting_approval where no worker can
// claim it, and a fresh approval is created and pointed at it. It reuses the same
// gate and approval machinery the reactive engine uses, so there is one gate, not
// a second. The capability token is derived (its hash stored) but never returned
// here — `approval link` re-derives it on demand.
func (s *Service) gatedApply(ctx context.Context, enq Enqueuer, deriver reactive.Deriver,
	in ApplyInput) (ApplyResult, error) {
	spec := queue.Spec{
		Runbook:          WorkspaceRunbook(in.Workspace),
		Kind:             executor.KindTofu,
		Trigger:          "manual",
		Argv:             executor.TofuArgv("apply", in.SavedPlan.Path),
		Dir:              in.TofuDir,
		RunSecretBinding: in.RunSecrets,
		ConcurrencyKey:   workspaceConcurrencyKey(in.Workspace),
		IdempotencyKey:   in.IdempotencyKey,
		Timeout:          in.Timeout,
	}
	job, err := enq.EnqueueGated(ctx, spec)
	if err != nil {
		return ApplyResult{}, err
	}

	_, hash, err := reactive.ApprovalToken(deriver, job.ID)
	if err != nil {
		return ApplyResult{}, err
	}
	now := s.now().UTC()
	artifactID := in.SavedPlan.ID
	approval, err := s.store.CreateApproval(ctx, store.Approval{
		JobID:        job.ID,
		PlanArtifact: &artifactID,
		Workspace:    in.Workspace,
		PlanSerial:   in.WorkspaceSerial,
		RequestedAt:  now,
		ExpiresAt:    now.Add(reactive.DefaultApprovalTTL),
		TokenHash:    hash,
	})
	if err != nil {
		return ApplyResult{}, err
	}

	return ApplyResult{
		Gated: true,
		Approval: GatedApproval{
			ApprovalID:     approval.ID,
			JobID:          job.ID,
			State:          approval.State,
			ExpiresAt:      approval.ExpiresAt,
			Policy:         in.Gate.Policy,
			PlanArtifactID: in.SavedPlan.ID,
		},
	}, nil
}
