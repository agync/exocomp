package mcp

import (
	"context"
	"encoding/json"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// driftView is the values-free drift projection: the newest plan job's outcome
// for a workspace. It carries resource addresses, the actions tofu would take,
// and the attribute KEYS involved — never a value (the executor already
// suppresses those, tofu_drift.go). drift is the world moving under tofu;
// pending is a config change waiting to apply. A workspace with no plan job yet
// reports has_plan false and empty lists.
type driftView struct {
	Workspace string                   `json:"workspace"`
	HasPlan   bool                     `json:"has_plan"`
	PlanJobID int64                    `json:"plan_job_id,omitempty"`
	Changes   bool                     `json:"changes"`
	Drift     []executor.ResourceDelta `json:"drift,omitempty"`
	Pending   []executor.ResourceDelta `json:"pending,omitempty"`
}

// readDrift finds the newest tofu plan job for a workspace and projects its
// recorded outcome. It reads the store the same way the CLI's planOutcome does
// (the plan driver's own parse, read back from the job event), never re-running
// tofu, and stays values-free. WorkspaceByName first turns a bad name into a
// not_found the taxonomy maps, rather than silently reporting "no plan".
func (s *Server) readDrift(ctx context.Context, name string) (string, any, error) {
	if _, err := s.store.WorkspaceByName(ctx, name); err != nil {
		return "", nil, err
	}
	jobs, err := s.store.ListJobs(ctx, store.JobFilter{Runbook: service.WorkspaceRunbook(name)})
	if err != nil {
		return "", nil, err
	}
	view := driftView{Workspace: name}
	// ListJobs is oldest-first; walk from the newest so the latest plan wins.
	for i := len(jobs) - 1; i >= 0; i-- {
		if jobs[i].Kind != executor.KindTofu {
			continue
		}
		outcome, ok, err := s.planOutcome(ctx, jobs[i].ID)
		if err != nil {
			return "", nil, err
		}
		if !ok {
			continue
		}
		view.HasPlan = true
		view.PlanJobID = jobs[i].ID
		view.Changes = outcome.Changes
		view.Drift = outcome.Drift
		view.Pending = outcome.Pending
		break
	}
	return kindWorkspaceDrift, view, nil
}

// planOutcome reads the structured plan outcome the tofu driver recorded on a
// plan job — the resource-level drift and pending changes it parsed from
// `tofu show -json`, values already suppressed. It mirrors the CLI's planOutcome
// read (a store read of the job event), which this lane may not call directly.
func (s *Server) planOutcome(ctx context.Context, jobID int64) (executor.TofuPlanOutcome, bool, error) {
	events, err := s.store.ListJobEvents(ctx, jobID)
	if err != nil {
		return executor.TofuPlanOutcome{}, false, err
	}
	for _, e := range events {
		if e.RawJSON == "" {
			continue
		}
		var envelope struct {
			Tofu *executor.TofuPlanOutcome `json:"tofu"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &envelope); err != nil || envelope.Tofu == nil {
			continue
		}
		if envelope.Tofu.Operation != "plan" {
			continue
		}
		return *envelope.Tofu, true, nil
	}
	return executor.TofuPlanOutcome{}, false, nil
}

// explainView projects why one event ended the way it did: the values-free
// event row plus the guardrail trace, reduced to step/passed/reason. The trace
// Detail field is dropped: it can carry a matched value, and the step and reason
// are what say which guardrail decided (invariant 4).
type explainView struct {
	Event wire.EventView `json:"event"`
	Trace []traceStep    `json:"trace,omitempty"`
}

type traceStep struct {
	Step   string `json:"step"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}

// readExplain projects the event and its recorded decision trace. It reuses the
// same reactive.Explain read the CLI's `event explain` uses.
func (s *Server) readExplain(ctx context.Context, id int64) (string, any, error) {
	x, err := reactive.Explain(ctx, s.store, id)
	if err != nil {
		return "", nil, err
	}
	view := explainView{Event: wire.NewEventView(x.Event)}
	for _, c := range x.Trace {
		view.Trace = append(view.Trace, traceStep{Step: c.Step, Passed: c.Passed, Reason: c.Reason})
	}
	return kindEventExplain, view, nil
}
