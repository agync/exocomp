package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// tailPoll is how often a CLI following a job the daemon is running re-reads
// its events and its state.
const tailPoll = 100 * time.Millisecond

// daemonRunning reports whether an `exocomp serve` process is up for this data
// dir. Two signals, because neither is sufficient alone: the recorded address
// answering /healthz proves a state backend is serving, which a plain
// `workspace plan` also does for the length of its own run, and the daemon
// lock proves the process behind it is the one draining the queue.
func daemonRunning(ctx context.Context, dir string) bool {
	if !serveLockHeld(dir) {
		return false
	}
	addr, err := api.ReadAddress(filepath.Join(dir, api.AddressFile))
	if err != nil {
		return false
	}
	return api.Reachable(ctx, addr)
}

// enqueueForDaemon hands one spec to the running daemon and follows the job it
// created to a terminal state. The CLI does not drain: two queues over one job
// table race for the row, and the daemon is the one that can build every
// kind's driver.
func enqueueForDaemon(ctx context.Context, st *store.Store, dir string, spec queue.Spec, out io.Writer) (
	store.Job, error) {
	jobs, err := enqueueAllForDaemon(ctx, st, dir, []queue.Spec{spec}, out)
	if err != nil {
		return store.Job{}, err
	}
	return jobs[0], nil
}

// enqueueAllForDaemon queues every spec before following any of them, so the
// daemon's workers run them side by side while this command reports them in
// the order it asked for.
func enqueueAllForDaemon(ctx context.Context, st *store.Store, dir string, specs []queue.Spec,
	out io.Writer) ([]store.Job, error) {
	q := queue.New(st, enqueueOnly{}, queue.Options{Workers: 1})

	ids := make([]int64, 0, len(specs))
	for _, spec := range specs {
		job, err := q.Enqueue(ctx, spec)
		if err != nil {
			return nil, err
		}
		_, _ = fmt.Fprintf(out, "job %d queued for the running daemon\n", job.ID)
		ids = append(ids, job.ID)
	}

	jobs := make([]store.Job, 0, len(ids))
	for _, id := range ids {
		job, err := tailDaemonJob(ctx, st, dir, id, out)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// tailDaemonJob prints a job's events as they land until the job reaches a
// terminal state. The queue writes a job's last event before the transition
// that ends it, so a terminal row means the events are complete.
//
// A daemon that dies without retiring its rows would otherwise leave this
// waiting forever, so the lock is rechecked whenever the job is not finished.
func tailDaemonJob(ctx context.Context, st *store.Store, dir string, jobID int64, out io.Writer) (
	store.Job, error) {
	ticker := time.NewTicker(tailPoll)
	defer ticker.Stop()

	seen := 0
	for {
		n, err := tailEvents(ctx, st, jobID, seen, out)
		if err != nil {
			return store.Job{}, err
		}
		seen = n

		job, err := st.GetJob(ctx, jobID)
		if err != nil {
			return store.Job{}, err
		}
		if job.State.Terminal() {
			if _, err := tailEvents(ctx, st, jobID, seen, out); err != nil {
				return store.Job{}, err
			}
			return job, nil
		}
		if !serveLockHeld(dir) {
			return store.Job{}, fmt.Errorf("job %d: the daemon stopped while the job was %s",
				jobID, job.State)
		}

		select {
		case <-ctx.Done():
			return store.Job{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// enqueueOnly is the driver of a queue this process only enqueues through. It
// is never drained; refusing loudly is what keeps a future caller from
// draining it by accident and racing the daemon for the row.
type enqueueOnly struct{}

// Run implements executor.Driver.
func (enqueueOnly) Run(context.Context, executor.Spec, executor.EventSink) (executor.Result, error) {
	return executor.Result{}, errors.New("queue: this process only enqueues; the daemon runs the job")
}
