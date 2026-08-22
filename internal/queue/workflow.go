package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// Step is one leg of a workflow: a job to run, plus the kind recorded in the
// ledger. The Spec is an ordinary queue Spec — the sequencer sets its
// idempotency key, so a resume re-resolves the same job rather than running the
// effect twice.
type Step struct {
	Kind string
	Spec Spec
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
			StepNo:         i + 1,
			Kind:           step.Kind,
			IdempotencyKey: spec.IdempotencyKey,
			JobID:          &id,
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
			return err
		}
	}
}

// ResumeAll continues every workflow the ledger still holds unfinished. It is
// the boot-sweep extension: after the job sweep retires the rows a dead process
// left, this picks up the workflows those jobs belonged to and runs their
// remaining steps — never the ones already succeeded. Errors from one workflow
// do not stop the others.
func (s *Sequencer) ResumeAll(ctx context.Context) (int, error) {
	ids, err := s.q.store.ListIncompleteWorkflows(ctx)
	if err != nil {
		return 0, err
	}
	resumed := 0
	var errs []error
	for _, id := range ids {
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

	// A step whose job already reached a terminal state is decided by that job,
	// not re-run: this is the crash-between-effect-and-ledger case and the
	// resume of a job the boot sweep retired to interrupted.
	if found && job.State.Terminal() {
		return s.recordOutcome(ctx, workflowID, step, job)
	}
	if !found {
		return fmt.Errorf("queue: workflow %s step %d has no job to run", workflowID, step.StepNo)
	}

	// The job is gated or already queued. Release a gated one into the queue;
	// one a crash left queued (released, but the ledger not yet advanced) is
	// left where it is. Then mark the ledger running, unless a prior process
	// already did.
	if job.State == store.StateAwaitingApproval {
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
