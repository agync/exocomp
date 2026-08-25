package reactive

import (
	"context"
	"fmt"
	"strings"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// outcomeScan bounds how many finished jobs one sweep looks back over, the same
// bound driftScan puts on the drift sweep.
const outcomeScan = 200

// SourceJobOutcome attributes the checkpoint event a job-failure page leaves
// behind. It is not a webhook source: like SourceDrift it names an internally
// raised condition, here "a job ended badly and the operator was paged".
const SourceJobOutcome = "job-outcome"

// failedJobStates are the terminal states a job lands in when it did not
// succeed and nobody chose to stop it: it failed, its own timeout killed it, or
// its process vanished (a crash or a daemon restart marks the running row
// interrupted). Cancelled is left out — a deliberate stop is not an outcome to
// page on. The per-job dedup keeps a restart that interrupts many rows at once
// from spamming: each job pages once, however many sweeps run.
var failedJobStates = []store.JobState{store.StateFailed, store.StateTimedOut, store.StateInterrupted}

// SweepJobOutcomes pages the operator once for every recently-finished job that
// ended in a terminal failure and has not already been paged on. Today a failed
// scheduled converge or backup runs completely silent; this closes that.
//
// Its idempotency is SweepDrift's, unchanged: the event table is the checkpoint.
// A page's dedup key names the job it paged on, so a job already reported is
// skipped without a cursor to keep, a column to add, or state to lose on a
// restart — the same job never pages twice across repeated sweeps or a daemon
// restart.
//
// A failed workflow step surfaces here through its own failed job (a step that
// ran carries a job id, and that job is in StateFailed), so this one scan covers
// both. A blocked step never ran and has no failure of its own to report — the
// earlier failed step that blocked it is the one that pages.
func (e *Engine) SweepJobOutcomes(ctx context.Context) (int, error) {
	jobs, err := e.store.ListJobs(ctx, store.JobFilter{
		States: failedJobStates,
		Limit:  outcomeScan,
	})
	if err != nil {
		return 0, err
	}

	raised := 0
	for _, job := range jobs {
		key := jobOutcomeDedupKey(job.ID)
		seen, err := e.store.CountEventsByDedupKey(ctx, key)
		if err != nil {
			return raised, err
		}
		if seen > 0 {
			continue
		}

		host, _ := WorkspaceOf(job.Runbook)
		lastErr := scrub(e.scrubber(), e.lastJobError(ctx, job.ID))
		e.notify(ctx, jobFailedNotification(job, host, lastErr))

		// The checkpoint row is written after the page, so a crash between the
		// two re-pages once on the next sweep — louder than a lost alert, the
		// same trade SweepDrift makes. CountEventsByDedupKey counts it whatever
		// its outcome, so this is what the next sweep skips on.
		if _, err := e.store.CreateEvent(ctx, store.Event{
			Source:     SourceJobOutcome,
			ReceivedAt: e.now(),
			DedupKey:   key,
			Outcome:    store.OutcomeMatched,
			Reason:     fmt.Sprintf("job %d ended %s; operator paged", job.ID, job.State),
			JobID:      &job.ID,
		}); err != nil {
			return raised, err
		}
		raised++
	}
	return raised, nil
}

// jobOutcomeDedupKey names the job a page reported on, so one failed job pages
// once however often the sweep runs. It mirrors driftDedupKey.
func jobOutcomeDedupKey(jobID int64) string {
	return fmt.Sprintf("%s:%d", SourceJobOutcome, jobID)
}

// lastJobError returns the message of the job's last error-level event, or the
// empty string when it recorded none. The stored message was already scrubbed
// when the run wrote it; the sweep scrubs again before it leaves the process.
func (e *Engine) lastJobError(ctx context.Context, jobID int64) string {
	events, err := e.store.LastJobEvents(ctx, jobID, 20)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Level == store.LevelError && strings.TrimSpace(events[i].Msg) != "" {
			return events[i].Msg
		}
	}
	return ""
}

// jobFailedNotification is the page a terminal failure raises: high priority,
// attributed by runbook and workspace so a scoped mute can hold it, carrying the
// outcome and the last error the run left.
func jobFailedNotification(job store.Job, host, lastErr string) notify.Notification {
	name := job.Runbook
	if name == "" {
		name = fmt.Sprintf("job %d", job.ID)
	}
	body := fmt.Sprintf("Job %d (%s) ended %s.", job.ID, job.Kind, job.State)
	if lastErr != "" {
		body += " Last error: " + lastErr
	}
	return notify.Notification{
		Title:    "job failed: " + name,
		Body:     body,
		Priority: notify.PriorityHigh,
		Tags:     []string{"rotating_light", fmt.Sprintf("id:%d", job.ID), "kind:job"},
		Click:    fmt.Sprintf("exocomp://jobs/%d", job.ID),
		Source:   job.Runbook,
		Host:     host,
	}
}
