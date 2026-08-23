package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// Spec describes work to enqueue. It carries no environment or secret
// material: those are resolved at execution time and never persisted.
type Spec struct {
	Runbook string
	Kind    string
	Trigger string
	Argv    []string
	Dir     string
	// VaultProject names the envelope derivation an ansible job's vault
	// password comes from, so whichever process claims the job can derive it.
	VaultProject string
	// RunSecretBinding names the run_secret set resolved to named env for the
	// run; Inventory names a registered inventory whose env_binding is resolved
	// alongside it. Both carry only names — the sealed values are resolved when
	// the job runs and never written to the row.
	RunSecretBinding string
	Inventory        string
	// ExtraVars are an ansible run's operator-supplied `-e key=val` variables,
	// carried to the job row so a daemon replays them and written to a file at
	// run time, never onto the ansible process's argv.
	ExtraVars map[string]string
	// Captures carries a bootstrap chain's env-decomposition wiring (the play's
	// capture targets and the workflow id) to the job row so a daemon that claims
	// or re-arms the step keeps it. Names and files only, never a value.
	Captures *store.JobCaptures
	// TolerateUnreachable opts an ansible run into the degraded outcome: a run
	// whose only non-success is unreachable hosts settles as a tolerated
	// degradation, not a failure. Carried to the job row so a daemon that claims
	// the job runs it under the same tolerance.
	TolerateUnreachable bool
	ConcurrencyKey      string
	DedupKey            string
	IdempotencyKey      string
	Timeout             time.Duration
}

// Options configures a Queue. The zero value is usable.
type Options struct {
	// Workers caps concurrently running jobs. Default 4.
	Workers int
	// DefaultTimeout applies to jobs that specify none. Zero means no limit.
	DefaultTimeout time.Duration
	// PollInterval bounds how long the queue sleeps when nothing wakes it.
	// Default one second.
	PollInterval time.Duration
	// Logger receives failures that are not the job's own, e.g. an event
	// that could not be written. Default slog.Default().
	Logger *slog.Logger
}

// Queue drains queued job rows, serializing jobs that share a concurrency
// key. It is the single execution path: guardrails live here, not in
// configuration (AGENTS.md invariant 5).
type Queue struct {
	store          *store.Store
	driver         executor.Driver
	workers        int
	defaultTimeout time.Duration
	pollInterval   time.Duration
	log            *slog.Logger
	wrapSink       func(executor.EventSink) executor.EventSink
	// claimGate reports whether NEW jobs may be claimed. A cordoned daemon
	// closes it (returns false) to stop taking work ahead of a planned reboot,
	// while jobs already launched run to completion — the gate is consulted only
	// before a claim, never against a running job. Nil means always open.
	claimGate func() bool

	// approver arms the approval gate for a workflow step that requires one. It
	// is the seam the Sequencer uses instead of releasing such a step: the
	// reactive engine (which owns the deriver and the same CreateApproval path a
	// single gated apply uses) registers itself here via SetStepApprover, so the
	// queue package never imports it. Nil leaves a require-approval step parked
	// with no way to advance — fail-closed — rather than released unapproved.
	approver StepApprover

	wake chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	enqueued map[int64]bool
	inflight int
}

// New returns a queue draining st through drv, adjusted by extra.
func New(st *store.Store, drv executor.Driver, opts Options, extra ...Option) *Queue {
	q := &Queue{
		store:          st,
		driver:         drv,
		workers:        opts.Workers,
		defaultTimeout: opts.DefaultTimeout,
		pollInterval:   opts.PollInterval,
		log:            opts.Logger,
		wake:           make(chan struct{}, 1),
		enqueued:       map[int64]bool{},
	}
	if q.workers <= 0 {
		q.workers = 4
	}
	if q.pollInterval <= 0 {
		q.pollInterval = time.Second
	}
	if q.log == nil {
		q.log = slog.Default()
	}
	for _, opt := range extra {
		opt(q)
	}
	return q
}

// Enqueue records a job in the queued state. When the spec carries an
// idempotency key that a job already holds, that job is returned unchanged
// instead of a second one being created.
func (q *Queue) Enqueue(ctx context.Context, spec Spec) (store.Job, error) {
	if len(spec.Argv) == 0 {
		return store.Job{}, errors.New("queue: enqueue: empty argv")
	}

	if spec.IdempotencyKey != "" {
		existing, err := q.store.JobByIdempotencyKey(ctx, spec.IdempotencyKey)
		switch {
		case err == nil:
			q.remember(existing.ID)
			return existing, nil
		case !errors.Is(err, store.ErrNotFound):
			return store.Job{}, err
		}
	}

	job, err := q.store.CreateJob(ctx, store.NewJob{
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
	if errors.Is(err, store.ErrDuplicate) && spec.IdempotencyKey != "" {
		// Another caller won the race on the same key.
		existing, getErr := q.store.JobByIdempotencyKey(ctx, spec.IdempotencyKey)
		if getErr == nil {
			q.remember(existing.ID)
		}
		return existing, getErr
	}
	if err != nil {
		return store.Job{}, err
	}

	q.remember(job.ID)
	q.notify()
	return job, nil
}

// StepApprover arms the approval gate for a workflow step that requires one:
// it creates the pending approval (minting its capability) exactly as a single
// gated apply does, so the step is released only by the same DecideApproval
// path. The queue package holds only this seam; the reactive engine, which owns
// the deriver, provides the implementation, keeping the approval machinery in
// one place and out of the queue's import graph.
type StepApprover interface {
	// ArmStepApproval ensures a pending approval exists for the gated job. It is
	// idempotent: an approval already recorded for the job is a no-op, so a
	// Sequencer that re-checks a parked step every pass mints and notifies once.
	ArmStepApproval(ctx context.Context, jobID int64) error
}

// SetStepApprover registers the approval-arming seam. The daemon's reactive
// engine calls it at construction (it already receives this queue), so the
// Sequencer built later over the same queue arms gated steps without any
// per-process wiring in the daemon's setup.
func (q *Queue) SetStepApprover(a StepApprover) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.approver = a
}

// stepApprover returns the registered approver under the queue's lock.
func (q *Queue) stepApprover() StepApprover {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.approver
}

// remember records a job this queue created, which is the set a Drain claims
// from and waits on.
func (q *Queue) remember(id int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueued[id] = true
}

// forget drops a job this queue ran to its end. The daemon enqueues for as
// long as it lives, so the set has to shrink as well as grow.
func (q *Queue) forget(id int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.enqueued, id)
}

func (q *Queue) mine() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := make([]int64, 0, len(q.enqueued))
	for id := range q.enqueued {
		ids = append(ids, id)
	}
	return ids
}

// Run drains every queued row until ctx is done, then waits for in-flight
// jobs. It is the daemon's loop: one process claims whatever reaches the job
// table, whichever process put it there. Cancelling ctx also cancels those
// jobs' subprocesses, which land in the cancelled state.
func (q *Queue) Run(ctx context.Context) error {
	return q.loop(ctx, true)
}

// Drain runs the jobs this queue enqueued and returns once every one of them
// is terminal. It is what a one-shot command uses instead of a long-lived
// worker, and it claims nothing else: a CLI that swept up a daemon's rows
// would run them through a driver built for its own job and fail them.
func (q *Queue) Drain(ctx context.Context) error {
	return q.loop(ctx, false)
}

// loop dispatches until it should stop. claimAny separates the daemon (claims
// every queued row, never idles) from a one-shot drain (claims only its own
// rows, and returns when they are done).
func (q *Queue) loop(ctx context.Context, claimAny bool) error {
	defer q.wg.Wait()

	ticker := time.NewTicker(q.pollInterval)
	defer ticker.Stop()

	for {
		if err := q.dispatch(ctx, claimAny); err != nil {
			return err
		}
		if !claimAny {
			idle, err := q.idle(ctx)
			if err != nil {
				return err
			}
			if idle {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-q.wake:
		case <-ticker.C:
		}
	}
}

// dispatch launches every claimable job a free worker slot allows.
func (q *Queue) dispatch(ctx context.Context, claimAny bool) error {
	for ctx.Err() == nil {
		if !q.takeSlot() {
			return nil
		}
		job, ok, err := q.claim(ctx, claimAny)
		if err != nil || !ok {
			q.finishSlot()
			return err
		}
		q.launch(ctx, job)
	}
	return nil
}

// claim moves the oldest runnable queued job to running. Jobs whose
// concurrency key is already held are skipped, not waited for; so is every
// row this queue did not enqueue when claimAny is false.
func (q *Queue) claim(ctx context.Context, claimAny bool) (store.Job, bool, error) {
	// A cordoned daemon claims nothing new: the row it would take stays queued
	// until the cordon lifts (or the next boot sweeps it), so a planned reboot
	// drains to empty instead of picking up more work.
	if q.claimGate != nil && !q.claimGate() {
		return store.Job{}, false, nil
	}
	jobs, err := q.store.ListJobs(ctx, store.JobFilter{
		States: []store.JobState{store.StateQueued},
		Limit:  512,
	})
	if err != nil {
		return store.Job{}, false, err
	}

	for _, job := range jobs {
		if !claimAny && !q.owns(job.ID) {
			continue
		}
		// The store decides: the queued -> running move and the concurrency
		// key check are one statement there, so a second process over the same
		// database cannot claim a key this one is holding.
		err := q.store.ClaimJob(ctx, job.ID)
		if err != nil {
			// A pending approval refuses the claim exactly like a held concurrency
			// key: skip the row and try the next, never run it. The gate holds even
			// if a mis-release left the job queued with its approval undecided.
			if errors.Is(err, store.ErrStateConflict) || errors.Is(err, store.ErrConcurrencyHeld) ||
				errors.Is(err, store.ErrApprovalPending) {
				continue
			}
			return store.Job{}, false, err
		}
		return job, true, nil
	}
	return store.Job{}, false, nil
}

func (q *Queue) owns(id int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.enqueued[id]
}

func (q *Queue) launch(ctx context.Context, job store.Job) {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		defer q.notify()
		defer q.finishSlot()
		defer q.forget(job.ID)

		q.execute(ctx, job)
	}()
}

func (q *Queue) execute(ctx context.Context, job store.Job) {
	var sink executor.EventSink = newStoreSink(q.store, job.ID, q.log)
	if q.wrapSink != nil {
		sink = q.wrapSink(sink)
	}
	sink.Emit(executor.Event{
		Level: executor.LevelSystem,
		Msg:   "running " + strings.Join(job.Args.Argv, " "),
	})

	timeout := time.Duration(job.Args.Timeout)
	if timeout <= 0 {
		timeout = q.defaultTimeout
	}

	res, runErr := q.driver.Run(ctx, executor.Spec{
		JobID:   job.ID,
		Kind:    job.Kind,
		Argv:    job.Args.Argv,
		Dir:     job.Args.Dir,
		Timeout: timeout,
	}, sink)

	// The job's own outcome must be recorded even when the queue is shutting
	// down, so finalisation does not run under the cancelled context.
	final := context.WithoutCancel(ctx)
	state, msg := outcome(res, runErr, ctx.Err())

	if runErr != nil {
		sink.Emit(executor.Event{Level: executor.LevelError, Msg: runErr.Error()})
	} else if err := q.store.SetJobExitCode(final, job.ID, res.ExitCode); err != nil {
		q.log.Error("record job exit code", "job", job.ID, "err", err)
	}

	// A degraded run settled as a tolerated non-failure: the driver reported it
	// (an unreachable-tolerant ansible run whose only non-success was offline
	// hosts). The state below stays succeeded so no consumer is disturbed; this
	// flag records that the success was degraded, not clean, and the driver has
	// already emitted the event naming the unreachable hosts.
	if runErr == nil && res.Degraded && state == store.StateSucceeded {
		if err := q.store.SetJobPartial(final, job.ID, true); err != nil {
			q.log.Error("record job partial", "job", job.ID, "err", err)
		}
	}

	sink.Emit(executor.Event{Level: executor.LevelSystem, Msg: msg})
	if err := q.store.TransitionJob(final, job.ID, store.StateRunning, state); err != nil {
		q.log.Error("finish job", "job", job.ID, "state", state, "err", err)
	}
}

// outcome maps a driver result onto the job state machine. Exit code alone
// decides success; everything else only explains the ending. A process that
// exited on its own keeps its exit code even if a cancel arrived meanwhile —
// cancellation only claims jobs the cancel actually stopped or prevented.
func outcome(res executor.Result, runErr, ctxErr error) (store.JobState, string) {
	cancelled := errors.Is(ctxErr, context.Canceled) && (runErr != nil || res.Signaled)

	switch {
	case res.TimedOut:
		return store.StateTimedOut, fmt.Sprintf("timed out after %s", res.Duration.Round(time.Millisecond))
	case cancelled:
		return store.StateCancelled, "cancelled"
	case runErr != nil:
		return store.StateFailed, "driver error: " + runErr.Error()
	case res.ExitCode == 0:
		return store.StateSucceeded, "succeeded (exit 0)"
	case res.Degraded:
		// A tolerated degradation: the process exited non-zero, but its only
		// non-success was a condition the operator opted to tolerate (ansible
		// unreachable hosts). It settles succeeded-but-degraded, not failed, so
		// it does not page — while the real exit code stays recorded.
		return store.StateSucceeded, fmt.Sprintf("succeeded (degraded: exit %d, %d host(s) unreachable, tolerated)",
			res.ExitCode, len(res.UnreachableHosts))
	default:
		return store.StateFailed, fmt.Sprintf("failed (exit %d)", res.ExitCode)
	}
}

// idle reports whether every job this queue enqueued has reached a terminal
// state. Global emptiness is the wrong question for a one-shot command: a
// daemon enqueueing on a schedule keeps the table non-empty forever, and a
// row another process claimed still has to be waited for.
func (q *Queue) idle(ctx context.Context) (bool, error) {
	q.mu.Lock()
	inflight := q.inflight
	q.mu.Unlock()
	if inflight > 0 {
		return false, nil
	}

	for _, id := range q.mine() {
		job, err := q.store.GetJob(ctx, id)
		if err != nil {
			return false, err
		}
		if !job.State.Terminal() {
			return false, nil
		}
	}
	return true, nil
}

// takeSlot reserves a worker slot when one is free.
func (q *Queue) takeSlot() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.inflight >= q.workers {
		return false
	}
	q.inflight++
	return true
}

func (q *Queue) finishSlot() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.inflight--
}

func (q *Queue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
