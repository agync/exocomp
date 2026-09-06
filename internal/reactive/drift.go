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

// driftScan bounds how many finished jobs one sweep looks back over.
const driftScan = 200

// SweepDrift turns every scheduled dry run that found changes into an event in
// the same pipeline every webhook goes through, so a rule can gate a
// remediation behind an approval on the strength of drift alone.
//
// Both halves of a homelab are swept here: a `tofu plan` that found changes and
// an ansible `--check` play whose recap reported changed tasks. They differ in
// what a drift subject is and in how the outcome is read back, and in nothing
// else — one loop, one checkpoint discipline, one page identity.
//
// The event table is its own checkpoint: a drift event's dedup key names the
// job that found the drift, so a job already reported is skipped without a
// cursor to keep, a column to add, or state to lose on a restart.
func (e *Engine) SweepDrift(ctx context.Context) (int, error) {
	jobs, err := e.store.ListJobs(ctx, store.JobFilter{
		States: []store.JobState{store.StateSucceeded, store.StateDegraded},
		Limit:  driftScan,
	})
	if err != nil {
		return 0, err // structural: the sweep cannot run at all
	}

	raised := 0
	for _, job := range jobs {
		n, keep := e.sweepStep(ctx, "sweep drift", fmt.Sprintf("job %d", job.ID), func() (int, error) {
			return e.sweepDriftJob(ctx, job)
		})
		if !keep {
			return raised, ctx.Err()
		}
		raised += n
	}
	return raised, nil
}

// sweepDriftJob settles one finished job: a tofu plan that found changes or an
// ansible check that changed tasks raises drift. Any other kind is a no-op.
func (e *Engine) sweepDriftJob(ctx context.Context, job store.Job) (int, error) {
	switch job.Kind {
	case executor.KindTofu:
		return e.sweepTofuPlan(ctx, job)
	case executor.KindAnsible:
		return e.sweepAnsibleCheck(ctx, job)
	default:
		return 0, nil
	}
}

// sweepTofuPlan settles one finished plan: it raises the workspace's drift, or
// clears the incident an earlier plan opened.
func (e *Engine) sweepTofuPlan(ctx context.Context, job store.Job) (int, error) {
	workspace, ok := WorkspaceOf(job.Runbook)
	if !ok {
		return 0, nil
	}

	key := driftDedupKey(workspace, job.ID)
	seen, err := e.store.CountEventsByDedupKey(ctx, key)
	if err != nil {
		return 0, err
	}
	if seen > 0 {
		return 0, nil
	}

	changes, err := e.planFoundChanges(ctx, job.ID)
	if err != nil {
		return 0, err
	}
	if !changes {
		// A clean plan is the other half of the story: if this workspace has an
		// open, unresolved drift incident, this plan clears it. Jobs come back
		// oldest first, so a drift and the clean plan that follows it settle in
		// order within one pass.
		openJob, open, err := e.latestOpenDrift(ctx, workspace)
		if err != nil || !open {
			return 0, err
		}
		return 0, e.clearDrift(ctx, workspace, openJob, job)
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
		return 0, err
	}
	if _, err := e.Ingest(ctx, ev, raw); err != nil {
		return 0, err
	}
	return 1, nil
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
