package queue

import (
	"context"
	"errors"
	"fmt"

	"exocomp.dev/exocomp/internal/store"
)

// EnqueueGated records a job that no worker may claim until an approval
// releases it. The row is written straight into awaiting_approval — never
// queued first — so a destructive action is not claimable for the instant
// between its creation and a transition.
//
// The gate is a queue property, not a UI convention: `claim` reads queued rows
// only, so every path into execution, daemon or CLI, is bound by it.
func (q *Queue) EnqueueGated(ctx context.Context, spec Spec) (store.Job, error) {
	if len(spec.Argv) == 0 {
		return store.Job{}, errors.New("queue: enqueue gated: empty argv")
	}

	if spec.IdempotencyKey != "" {
		existing, err := q.store.JobByIdempotencyKey(ctx, spec.IdempotencyKey)
		switch {
		case err == nil:
			return existing, nil
		case !errors.Is(err, store.ErrNotFound):
			return store.Job{}, err
		}
	}

	return q.store.CreateGatedJob(ctx, store.NewJob{
		Runbook: spec.Runbook,
		Kind:    spec.Kind,
		Trigger: spec.Trigger,
		Args: store.JobArgs{
			Argv:                spec.Argv,
			Dir:                 spec.Dir,
			Timeout:             store.Duration(spec.Timeout),
			VaultProject:        spec.VaultProject,
			RunSecretBinding:    spec.RunSecretBinding,
			Inventory:           spec.Inventory,
			ExtraVars:           spec.ExtraVars,
			TolerateUnreachable: spec.TolerateUnreachable,
			Captures:            spec.Captures,
		},
		ConcurrencyKey: spec.ConcurrencyKey,
		DedupKey:       spec.DedupKey,
		IdempotencyKey: spec.IdempotencyKey,
	})
}

// Release moves an approved job into the queue, where the daemon claims it
// like any other row.
//
// It deliberately does not add the job to the set a Drain waits on: the
// process approving may be a CLI whose driver cannot serve the job's kind, and
// a released job belongs to whichever daemon is draining, not to whoever
// tapped the link.
func (q *Queue) Release(ctx context.Context, id int64) error {
	err := q.store.TransitionJob(ctx, id, store.StateAwaitingApproval, store.StateQueued)
	if err != nil {
		return fmt.Errorf("queue: release job %d: %w", id, err)
	}
	q.notify()
	return nil
}

// Withdraw retires a gated job that will never run — rejected, expired, or
// overtaken by the alert resolving. Cancelled is the terminal state for work
// a human, or a deadline, decided against.
func (q *Queue) Withdraw(ctx context.Context, id int64) error {
	err := q.store.TransitionJob(ctx, id, store.StateAwaitingApproval, store.StateCancelled)
	if err != nil {
		return fmt.Errorf("queue: withdraw job %d: %w", id, err)
	}
	return nil
}
