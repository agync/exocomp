package reactive

import (
	"context"
	"errors"
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

// SourceNotifyRequest attributes a one-shot "notify me" request: an operator
// asked, through POST /jobs/{id}/notify, to be paged when a specific job reaches
// its terminal state, even one that ends cleanly (which the failure sweep above
// never pages on). The row is the durable, attributed record the daemon reads;
// the page it raises shares the SourceJobOutcome checkpoint, so a job that also
// fails is never paged twice.
const SourceNotifyRequest = "notify-request"

// NotifyRequestDedupKey names the job a one-shot notify request is registered
// for, so a second request for the same job is idempotent rather than a second
// page.
func NotifyRequestDedupKey(jobID int64) string {
	return fmt.Sprintf("%s:%d", SourceNotifyRequest, jobID)
}

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
		return 0, err // structural: the sweep cannot run at all
	}

	raised := 0
	for _, job := range jobs {
		n, keep := e.sweepStep(ctx, "sweep outcomes", fmt.Sprintf("job %d", job.ID), func() (int, error) {
			key := jobOutcomeDedupKey(job.ID)
			seen, err := e.store.CountEventsByDedupKey(ctx, key)
			if err != nil {
				return 0, err
			}
			if seen > 0 {
				return 0, nil
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
				return 0, err
			}
			return 1, nil
		})
		if !keep {
			return raised, ctx.Err()
		}
		raised += n
	}
	return raised, nil
}

// jobOutcomeDedupKey names the job a page reported on, so one failed job pages
// once however often the sweep runs. It mirrors driftDedupKey.
func jobOutcomeDedupKey(jobID int64) string {
	return fmt.Sprintf("%s:%d", SourceJobOutcome, jobID)
}

// SweepJobNotifyRequests pages once for every job an operator asked to be
// notified about (POST /jobs/{id}/notify) that has since reached a terminal
// state. Unlike SweepJobOutcomes it pages on a clean finish too — the operator
// asked to know the outcome, whatever it is.
//
// It shares SweepJobOutcomes' checkpoint: a job whose failure was already paged
// (its jobOutcomeDedupKey row exists) is not paged again, so a requested job
// that also fails yields exactly one page — the failure page that already went
// out — never two. A requested job that ends any other way (succeeded,
// cancelled) has no such checkpoint yet, so this raises the one page and writes
// the same checkpoint, which then also fulfils the request.
func (e *Engine) SweepJobNotifyRequests(ctx context.Context) (int, error) {
	reqs, err := e.store.ListEvents(ctx, store.EventFilter{
		Source: SourceNotifyRequest,
		Limit:  outcomeScan,
	})
	if err != nil {
		return 0, err
	}

	paged := 0
	for _, req := range reqs {
		if req.JobID == nil {
			continue
		}
		jobID := *req.JobID
		n, keep := e.sweepStep(ctx, "sweep notify", fmt.Sprintf("job %d", jobID), func() (int, error) {
			job, err := e.store.GetJob(ctx, jobID)
			if errors.Is(err, store.ErrNotFound) {
				return 0, nil
			}
			if err != nil {
				return 0, err
			}
			if !job.State.Terminal() {
				return 0, nil
			}

			seen, err := e.store.CountEventsByDedupKey(ctx, jobOutcomeDedupKey(jobID))
			if err != nil {
				return 0, err
			}
			if seen > 0 {
				// Already paged by the failure sweep (or a prior pass); the request is
				// covered by that page, so this one stays silent.
				return 0, nil
			}

			host, _ := WorkspaceOf(job.Runbook)
			e.notify(ctx, jobDoneNotification(job, host))

			// The shared checkpoint is written after the page, so a crash between the
			// two re-pages once on the next sweep — the same trade SweepJobOutcomes
			// makes. It also stops the failure sweep from paging the same job again.
			if _, err := e.store.CreateEvent(ctx, store.Event{
				Source:     SourceJobOutcome,
				ReceivedAt: e.now(),
				DedupKey:   jobOutcomeDedupKey(jobID),
				Outcome:    store.OutcomeMatched,
				Reason:     fmt.Sprintf("job %d ended %s; notify request paged", jobID, job.State),
				JobID:      &jobID,
			}); err != nil {
				return 0, err
			}
			return 1, nil
		})
		if !keep {
			return paged, ctx.Err()
		}
		paged += n
	}
	return paged, nil
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

// jobDoneNotification is the page a one-shot notify request raises when its job
// reaches any terminal state. It is default priority — the operator opted in, so
// this is an answer to a question they asked, not an alarm — and attributed by
// runbook and workspace like the failure page, so a scoped mute can hold it.
func jobDoneNotification(job store.Job, host string) notify.Notification {
	name := job.Runbook
	if name == "" {
		name = fmt.Sprintf("job %d", job.ID)
	}
	return notify.Notification{
		Title:    "job " + string(job.State) + ": " + name,
		Body:     fmt.Sprintf("Job %d (%s) ended %s — you asked to be notified.", job.ID, job.Kind, job.State),
		Priority: notify.PriorityDefault,
		Tags:     []string{fmt.Sprintf("id:%d", job.ID), "kind:job"},
		Click:    fmt.Sprintf("exocomp://jobs/%d", job.ID),
		Source:   job.Runbook,
		Host:     host,
	}
}
