package wire

import "exocomp.dev/exocomp/internal/store"

// KindWorkflow is the kind of a step-ledger workflow's status (a `handoff
// status` answer today).
const KindWorkflow = "workflow.status"

// WorkflowStepView projects one step-ledger row: its position, kind, status,
// and the job that ran it (nil while a step has not yet claimed one — pending
// or blocked).
type WorkflowStepView struct {
	StepNo int    `json:"step_no"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	JobID  *int64 `json:"job_id,omitempty"`
}

// WorkflowView is a workflow's step ledger: its derived overall state plus
// each step in ledger order.
type WorkflowView struct {
	ID    string             `json:"id"`
	State string             `json:"state"`
	Steps []WorkflowStepView `json:"steps"`
}

// NewWorkflowView projects a workflow's step ledger. State is derived with the
// identical store.WorkflowStatus the text renderer calls, so JSON and text can
// never disagree on whether a workflow is still running.
func NewWorkflowView(id string, steps []store.WorkflowStep) WorkflowView {
	out := make([]WorkflowStepView, len(steps))
	for i, s := range steps {
		out[i] = WorkflowStepView{StepNo: s.StepNo, Kind: s.Kind, Status: string(s.Status), JobID: s.JobID}
	}
	return WorkflowView{ID: id, State: string(store.WorkflowStatus(steps)), Steps: out}
}
