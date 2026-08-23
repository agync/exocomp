package reactive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// driftScan bounds how many finished tofu jobs one sweep looks back over.
const driftScan = 200

// SweepDrift turns every scheduled plan that found changes into an event in
// the same pipeline every webhook goes through, so a rule can gate an apply
// behind an approval on the strength of drift alone.
//
// The event table is its own checkpoint: a drift event's dedup key names the
// job that found the drift, so a job already reported is skipped without a
// cursor to keep, a column to add, or state to lose on a restart.
func (e *Engine) SweepDrift(ctx context.Context) (int, error) {
	jobs, err := e.store.ListJobs(ctx, store.JobFilter{
		States: []store.JobState{store.StateSucceeded},
		Limit:  driftScan,
	})
	if err != nil {
		return 0, err
	}

	raised := 0
	for _, job := range jobs {
		if job.Kind != executor.KindTofu {
			continue
		}
		workspace, ok := WorkspaceOf(job.Runbook)
		if !ok {
			continue
		}

		key := driftDedupKey(workspace, job.ID)
		seen, err := e.store.CountEventsByDedupKey(ctx, key)
		if err != nil {
			return raised, err
		}
		if seen > 0 {
			continue
		}

		changes, err := e.planFoundChanges(ctx, job.ID)
		if err != nil {
			return raised, err
		}
		if !changes {
			// A clean plan is the other half of the story: if this workspace has
			// an open, unresolved drift incident, this plan clears it. Jobs come
			// back oldest first, so a drift and the clean plan that follows it
			// settle in order within one pass.
			openJob, open, err := e.latestOpenDrift(ctx, workspace)
			if err != nil {
				return raised, err
			}
			if open {
				if err := e.clearDrift(ctx, workspace, openJob, job); err != nil {
					return raised, err
				}
			}
			continue
		}

		// Page the workspace's per-workspace drift incident before the checkpoint
		// (the ingested event) lands, so a crash re-pages once — the louder trade,
		// symmetric with clearDrift. The close reuses this Source/Host.
		e.notify(ctx, driftOpenNotification(workspace, job))

		ev := driftEvent(workspace, job, key, e.now())
		raw, err := json.Marshal(map[string]any{
			"workspace": workspace, "job": job.ID, "runbook": job.Runbook,
		})
		if err != nil {
			return raised, err
		}
		if _, err := e.Ingest(ctx, ev, raw); err != nil {
			return raised, err
		}
		raised++
	}
	return raised, nil
}

// driftDedupKey names the job that found the drift, so one plan raises one
// event however often the sweep runs. It deliberately does not collapse two
// plans of the same workspace: each plan is a fresh observation, and the
// dedup window is what decides whether the second one is worth acting on.
func driftDedupKey(workspace string, jobID int64) string {
	return fmt.Sprintf("%s:%s:%d", SourceDrift, workspace, jobID)
}

// driftEvent is what a plan that found changes looks like once it is in the
// pipeline: an alert whose name is the workspace's drift, so a CEL rule reads
// it the same way it reads anything Alertmanager sent.
func driftEvent(workspace string, job store.Job, key string, now time.Time) Event {
	return Event{
		Source:      SourceDrift,
		Kind:        "drift",
		Name:        "TofuDrift",
		Status:      StatusFiring,
		Severity:    "warning",
		Host:        workspace,
		Fingerprint: key,
		Summary:     fmt.Sprintf("workspace %s has drifted: job %d planned changes", workspace, job.ID),
		Labels: map[string]string{
			"workspace": workspace,
			"job":       fmt.Sprint(job.ID),
			"trigger":   job.Trigger,
		},
		Annotations: map[string]string{},
		ReceivedAt:  now,
		DedupKey:    key,
	}
}

// planFoundChanges reads the plan outcome back out of a job's own events,
// which is where the tofu driver put it: exit code 2 is neither success nor
// failure and has nowhere else durable to live.
func (e *Engine) planFoundChanges(ctx context.Context, jobID int64) (bool, error) {
	events, err := e.store.ListJobEvents(ctx, jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	for _, ev := range events {
		if changes, ok := executor.TofuPlanChanges(ev.RawJSON); ok {
			return changes, nil
		}
	}
	return false, nil
}
