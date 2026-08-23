package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// inlineOnlyKinds is the set of step kinds a daemon's ResumeAll must never
// re-drive. Some workflows run their steps inline over a driver only the
// operator command builds — the upgrade choreography's upgradeDriver — which
// the daemon's queue driver does not register. A hard crash mid-upgrade leaves
// such a workflow incomplete in the ledger; without this, the boot ResumeAll
// would hand its steps to a daemon driver that cannot execute the kind. The
// owning command registers its kinds at init, so the queue stays a generic
// executor with no compiled-in knowledge of upgrade.
var (
	inlineOnlyMu    sync.RWMutex
	inlineOnlyKinds = map[string]struct{}{}
)

// RegisterInlineOnlyKind marks step kinds as operator-inline: a workflow whose
// ledger carries any of them is left untouched by ResumeAll rather than
// re-driven by a daemon. Call it from an init function of the command that owns
// the kind. Safe for concurrent use.
func RegisterInlineOnlyKind(kinds ...string) {
	inlineOnlyMu.Lock()
	defer inlineOnlyMu.Unlock()
	for _, k := range kinds {
		inlineOnlyKinds[k] = struct{}{}
	}
}

// workflowInlineOnly reports whether any of a workflow's steps carry a kind
// registered as operator-inline, so ResumeAll can skip the whole workflow: a
// daemon cannot drive it, and re-arming one step of it would strand the rest.
func workflowInlineOnly(steps []store.WorkflowStep) bool {
	inlineOnlyMu.RLock()
	defer inlineOnlyMu.RUnlock()
	for _, step := range steps {
		if _, ok := inlineOnlyKinds[step.Kind]; ok {
			return true
		}
	}
	return false
}

// Step is one leg of a workflow: a job to run, plus the kind recorded in the
// ledger. The Spec is an ordinary queue Spec — the sequencer sets its
// idempotency key, so a resume re-resolves the same job rather than running the
// effect twice.
type Step struct {
	Kind string
	Spec Spec
	// RequireApproval gates this step behind an operator decision: the Sequencer
	// arms a pending approval when the step becomes current and does NOT release
	// it, so a running daemon cannot claim the step's job until the approval
	// settles — the durable gate a single destructive apply already has.
	RequireApproval bool
}

// Sequencer runs an ordered list of steps over the existing queue, one at a
// time, advancing to step N+1 only once step N reached succeeded (ADR 0008).
// It is not a second runtime: every step is an ordinary queue job, and the
// queue's own workers execute it. The sequencer only decides what to release
// next and records each step's outcome in the ledger.
type Sequencer struct {
	q   *Queue
	log *slog.Logger
}

// NewSequencer returns a sequencer driving workflows over q.
func NewSequencer(q *Queue) *Sequencer {
	return &Sequencer{q: q, log: q.log}
}

// stepResult is the ledger's record of how a step's job ended. Exit code alone
// decides success (AGENTS.md invariant 3); the rest only explains it.
type stepResult struct {
	JobID    int64  `json:"job_id"`
	JobState string `json:"job_state"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// Start creates a workflow: each step becomes a gated job the queue will not
// claim until released, and a pending ledger row. Gating the jobs up front lets
// the ledger reference a durable job — argv and all — that a resume in a later
// process can release, without the sequencer re-deriving anything.
//
// Start is idempotent on workflowID: a second call over a workflow that already
// has ledger rows is a no-op, and the per-step idempotency keys keep the gated
// jobs from being duplicated.
func (s *Sequencer) Start(ctx context.Context, workflowID string, steps []Step) error {
	if workflowID == "" {
		return errors.New("queue: start workflow: empty workflow id")
	}
	if len(steps) == 0 {
		return errors.New("queue: start workflow: no steps")
	}
	if _, err := s.q.store.GetWorkflow(ctx, workflowID); err == nil {
		return nil // already created
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	ledger := make([]store.NewWorkflowStep, 0, len(steps))
	for i, step := range steps {
		spec := step.Spec
		spec.IdempotencyKey = stepKey(workflowID, i+1)
		job, err := s.q.EnqueueGated(ctx, spec)
		if err != nil {
			return fmt.Errorf("queue: start workflow %s: step %d: %w", workflowID, i+1, err)
		}
		id := job.ID
		ledger = append(ledger, store.NewWorkflowStep{
			StepNo:          i + 1,
			Kind:            step.Kind,
			IdempotencyKey:  spec.IdempotencyKey,
			JobID:           &id,
			RequireApproval: step.RequireApproval,
		})
	}
	return s.q.store.CreateWorkflow(ctx, workflowID, ledger)
}

// Run drives a workflow to a terminal end: it runs the lowest unfinished step
// whose predecessors all succeeded, records its outcome, and repeats. It
// returns nil once every step is succeeded, or once a failure has left the rest
// blocked — both readable from the ledger afterward. A succeeded step is never
// re-run, which is what makes Run safe to call again on a resumed workflow.
func (s *Sequencer) Run(ctx context.Context, workflowID string) error {
	for {
		steps, err := s.q.store.GetWorkflow(ctx, workflowID)
		if err != nil {
			return err
		}

		next, ok := nextStep(steps)
		if !ok {
			return nil // every step terminal: succeeded, or failed with the rest blocked
		}
		if err := s.runStep(ctx, workflowID, next); err != nil {
			if errors.Is(err, errAwaitingApproval) {
				// The current step is parked on an unsettled approval. Stop this
				// pass rather than spin: the next drive re-checks, and an approve
				// (which releases the job) or a reject (which cancels it) is what
				// lets the step advance. The workflow is not terminal, only waiting.
				return nil
			}
			return err
		}
	}
}

// errAwaitingApproval signals that runStep parked the current step on an
// unsettled approval: it armed the gate and left the job awaiting_approval
// rather than releasing it. Run treats it as "nothing more to do this pass",
// not a failure.
var errAwaitingApproval = errors.New("queue: workflow step awaiting approval")

// ResumeAll continues every workflow the ledger still holds unfinished. It is
// the boot-sweep extension: after the job sweep retires the rows a dead process
// left, this picks up the workflows those jobs belonged to and runs their
// remaining steps — never the ones already succeeded. Errors from one workflow
// do not stop the others.
//
// Operator-inline workflows are skipped, not run: a workflow whose steps use a
// kind only an operator command's driver executes (the upgrade choreography) is
// left exactly as the crash left it, so the daemon never trips over an
// interrupted upgrade. The operator's own re-run or `rollback` settles it.
func (s *Sequencer) ResumeAll(ctx context.Context) (int, error) {
	ids, err := s.q.store.ListIncompleteWorkflows(ctx)
	if err != nil {
		return 0, err
	}
	resumed := 0
	var errs []error
	for _, id := range ids {
		steps, err := s.q.store.GetWorkflow(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("resume workflow %s: %w", id, err))
			continue
		}
		if workflowInlineOnly(steps) {
			continue // operator-inline (upgrade): not the daemon's to drive
		}
		if err := s.Run(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("resume workflow %s: %w", id, err))
			continue
		}
		resumed++
	}
	return resumed, errors.Join(errs...)
}

// nextStep returns the step Run should act on: the first that is not succeeded.
// A failed or blocked step there means the workflow already ended in failure,
// so there is nothing left to run.
func nextStep(steps []store.WorkflowStep) (store.WorkflowStep, bool) {
	for _, step := range steps {
		if step.Status == store.StepSucceeded {
			continue
		}
		if step.Status == store.StepFailed || step.Status == store.StepBlocked {
			return store.WorkflowStep{}, false
		}
		return step, true // pending or running
	}
	return store.WorkflowStep{}, false
}

// runStep runs one step to a terminal ledger status. It reconciles the step
// against its job first, so a step a crash left mid-flight resolves from what
// the job actually did rather than being run a second time.
func (s *Sequencer) runStep(ctx context.Context, workflowID string, step store.WorkflowStep) error {
	job, found, err := s.findJob(ctx, step)
	if err != nil {
		return err
	}

	// A step whose job reached a terminal state that is a DECISION — succeeded,
	// failed on its own exit code, or timed out — is settled by that job and not
	// re-run: this is the crash-between-effect-and-ledger case, where the job
	// finished before the process died.
	//
	// A job that did NOT reach a decision — the boot sweep retired it to
	// interrupted, or a shutdown cancelled it mid-run — means the process died
	// under the step before it could finish. The ledger's contract is to resume
	// (ADR 0008), so the step is re-armed and run again rather than recorded as
	// a failure that would block the rest of the workflow.
	if found && job.State.Terminal() {
		if !nonDecision(job) {
			return s.recordOutcome(ctx, workflowID, step, job)
		}
		rearmed, err := s.rearmStep(ctx, workflowID, step, job)
		if err != nil {
			return err
		}
		// Fall through with the fresh gated job in a pending step: the code
		// below releases it, marks the ledger running, and waits for it.
		job = rearmed
		step.Status = store.StepPending
		found = true
	}
	if !found {
		return fmt.Errorf("queue: workflow %s step %d has no job to run", workflowID, step.StepNo)
	}

	// The job is gated or already queued. A gated step that requires operator
	// approval is NOT released here: it is armed with a pending approval and left
	// awaiting_approval, so only a DecideApproval settles it — the same gate a
	// single destructive apply passes through, now honored mid-chain. Any other
	// gated step is the Sequencer's own to release into the queue; one a crash
	// left queued (released, but the ledger not yet advanced) is left where it is.
	if job.State == store.StateAwaitingApproval {
		if step.RequireApproval {
			if err := s.armStepApproval(ctx, job.ID); err != nil {
				return err
			}
			return errAwaitingApproval
		}
		if err := s.q.Release(ctx, job.ID); err != nil {
			return err
		}
	}
	if step.Status == store.StepPending {
		if err := s.q.store.StartStep(ctx, workflowID, step.StepNo, job.ID); err != nil {
			if errors.Is(err, store.ErrStateConflict) {
				return nil // another driver advanced it; re-read on the next loop
			}
			return err
		}
	}

	final, err := s.waitTerminal(ctx, job.ID)
	if err != nil {
		return err
	}
	return s.recordOutcome(ctx, workflowID, step, final)
}

// armStepApproval creates the pending approval a gated step waits on, through
// the registered approver (the reactive engine's ArmStepApproval, which mints
// the capability and records the approval the same way a single gated apply
// does). It is idempotent, so a step re-checked every drive pass mints once.
//
// With no approver registered the step stays parked with no approval to settle
// it — fail-closed. That is deliberate: releasing a require-approval step
// because the arming seam is absent would be the very defeat this closes, so the
// absence blocks the step rather than opening it.
func (s *Sequencer) armStepApproval(ctx context.Context, jobID int64) error {
	approver := s.q.stepApprover()
	if approver == nil {
		s.log.Warn("workflow step requires approval but no approver is registered; step is parked",
			"job", jobID)
		return nil
	}
	return approver.ArmStepApproval(ctx, jobID)
}

// nonDecision reports whether a terminal job ended without deciding the step's
// outcome, so a resume should re-run it rather than settle on it. Two states
// qualify: interrupted (a crash the boot sweep retired, or shutdown retiring a
// queued row), and cancelled AFTER the job had started running (a shutdown that
// killed a running step). A cancelled job that never started is an approval the
// gate denied — a real decision — so StartedAt is what tells the two apart.
func nonDecision(job store.Job) bool {
	switch job.State {
	case store.StateInterrupted:
		return true
	case store.StateCancelled:
		return job.StartedAt != nil
	default:
		return false
	}
}

// rearmStep enqueues a fresh gated job from an interrupted step's own spec,
// under a new idempotency key, and retargets the ledger step back to pending so
// the caller can release and run it. The old job stays terminal as history; the
// step's effect is expected to be idempotent (ADR 0008), so re-running a swap,
// an apply, or a play that a crash cut short converges rather than duplicates.
func (s *Sequencer) rearmStep(ctx context.Context, workflowID string, step store.WorkflowStep, job store.Job) (store.Job, error) {
	newKey := fmt.Sprintf("%s:r%d", step.IdempotencyKey, time.Now().UTC().UnixNano())
	// The re-armed job must carry EVERY durable replay field the original held,
	// not a subset: a field dropped here changes how the step runs on resume.
	// Captures is the field this lane exists for — a bootstrap play that lost it
	// would seal nothing and leave the apply's ab:<workflow> binding empty — and
	// ExtraVars/TolerateUnreachable are dropped by the same omission, so they are
	// carried too rather than left for a later crash to expose.
	newJob, err := s.q.EnqueueGated(ctx, Spec{
		Runbook:             job.Runbook,
		Kind:                job.Kind,
		Trigger:             job.Trigger,
		Argv:                job.Args.Argv,
		Dir:                 job.Args.Dir,
		VaultProject:        job.Args.VaultProject,
		RunSecretBinding:    job.Args.RunSecretBinding,
		Inventory:           job.Args.Inventory,
		ExtraVars:           job.Args.ExtraVars,
		TolerateUnreachable: job.Args.TolerateUnreachable,
		Captures:            job.Args.Captures,
		ConcurrencyKey:      job.ConcurrencyKey,
		Timeout:             time.Duration(job.Args.Timeout),
		IdempotencyKey:      newKey,
	})
	if err != nil {
		return store.Job{}, fmt.Errorf("queue: workflow %s step %d: re-enqueue: %w",
			workflowID, step.StepNo, err)
	}
	if err := s.q.store.RetargetStep(ctx, workflowID, step.StepNo, newKey); err != nil {
		return store.Job{}, err
	}
	return newJob, nil
}

// recordOutcome writes a finished step's status from its job's terminal state:
// succeeded on exit 0, failed otherwise, with the failure blocking the steps
// that were waiting on it. The write is guarded on the running status, so a
// duplicate — two drivers finishing the same step, or a reconcile of a step a
// prior process already recorded — is a no-op rather than a second effect.
func (s *Sequencer) recordOutcome(ctx context.Context, workflowID string, step store.WorkflowStep, job store.Job) error {
	status := store.StepFailed
	if job.State == store.StateSucceeded {
		status = store.StepSucceeded
	}

	result, err := json.Marshal(stepResult{
		JobID:    job.ID,
		JobState: string(job.State),
		ExitCode: job.ExitCode,
	})
	if err != nil {
		return fmt.Errorf("queue: workflow %s step %d: encode result: %w", workflowID, step.StepNo, err)
	}

	// A step found already-running at reconcile time needs the ledger moved
	// into running before it can be completed; the guard on the pending status
	// makes this a no-op when the step is already running.
	if step.Status == store.StepPending {
		if err := s.q.store.StartStep(ctx, workflowID, step.StepNo, job.ID); err != nil &&
			!errors.Is(err, store.ErrStateConflict) {
			return err
		}
	}

	if err := s.q.store.CompleteStep(ctx, workflowID, step.StepNo, status, string(result)); err != nil {
		if errors.Is(err, store.ErrStateConflict) {
			return nil // already recorded
		}
		return err
	}

	if status == store.StepFailed {
		if _, err := s.q.store.BlockPendingSteps(ctx, workflowID); err != nil {
			return err
		}
	}
	return nil
}

// findJob locates a step's job by its recorded id, falling back to the
// idempotency key the step was enqueued under.
func (s *Sequencer) findJob(ctx context.Context, step store.WorkflowStep) (store.Job, bool, error) {
	if step.JobID != nil {
		job, err := s.q.store.GetJob(ctx, *step.JobID)
		switch {
		case err == nil:
			return job, true, nil
		case !errors.Is(err, store.ErrNotFound):
			return store.Job{}, false, err
		}
	}
	if step.IdempotencyKey != "" {
		job, err := s.q.store.JobByIdempotencyKey(ctx, step.IdempotencyKey)
		switch {
		case err == nil:
			return job, true, nil
		case !errors.Is(err, store.ErrNotFound):
			return store.Job{}, false, err
		}
	}
	return store.Job{}, false, nil
}

// waitTerminal blocks until a job reaches a terminal state or ctx ends. The
// queue's workers run the job; the sequencer only watches for its end.
func (s *Sequencer) waitTerminal(ctx context.Context, jobID int64) (store.Job, error) {
	for {
		job, err := s.q.store.GetJob(ctx, jobID)
		if err != nil {
			return store.Job{}, err
		}
		if job.State.Terminal() {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return store.Job{}, ctx.Err()
		case <-time.After(s.q.pollInterval):
		}
	}
}

func stepKey(workflowID string, stepNo int) string {
	return fmt.Sprintf("wf:%s:step:%d", workflowID, stepNo)
}
