package wire_test

import (
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func TestGoldenWorkflowRunning(t *testing.T) {
	steps := []store.WorkflowStep{
		{WorkflowID: "handoff:web:1", StepNo: 1, Kind: "tofu-apply", Status: store.StepSucceeded, JobID: ptrInt64(7)},
		{WorkflowID: "handoff:web:1", StepNo: 2, Kind: "ansible-play", Status: store.StepRunning, JobID: ptrInt64(8)},
	}
	got, err := wire.MarshalSuccess(wire.KindWorkflow, wire.NewWorkflowView("handoff:web:1", steps))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"workflow.status","data":{"id":"handoff:web:1","state":"running","steps":[` +
		`{"step_no":1,"kind":"tofu-apply","status":"succeeded","job_id":7},` +
		`{"step_no":2,"kind":"ansible-play","status":"running","job_id":8}]}}` + "\n"
	if string(got) != want {
		t.Fatalf("workflow bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

func TestGoldenWorkflowBlockedStepHasNoJob(t *testing.T) {
	steps := []store.WorkflowStep{
		{WorkflowID: "handoff:web:1", StepNo: 1, Kind: "tofu-apply", Status: store.StepFailed, JobID: ptrInt64(7)},
		{WorkflowID: "handoff:web:1", StepNo: 2, Kind: "ansible-play", Status: store.StepBlocked},
	}
	got, err := wire.MarshalSuccess(wire.KindWorkflow, wire.NewWorkflowView("handoff:web:1", steps))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"workflow.status","data":{"id":"handoff:web:1","state":"failed","steps":[` +
		`{"step_no":1,"kind":"tofu-apply","status":"failed","job_id":7},` +
		`{"step_no":2,"kind":"ansible-play","status":"blocked"}]}}` + "\n"
	if string(got) != want {
		t.Fatalf("blocked-step bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

func ptrInt64(n int64) *int64 { return &n }
