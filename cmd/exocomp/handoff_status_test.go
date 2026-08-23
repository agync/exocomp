package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// seedTwoStepWorkflow creates a workflow of two pending steps directly in the
// store, the same shape queue.Sequencer.Start would leave a fresh handoff in,
// without needing a running daemon.
func seedTwoStepWorkflow(t *testing.T, st *store.Store, id string) {
	t.Helper()
	steps := []store.NewWorkflowStep{
		{StepNo: 1, Kind: "tofu-apply", IdempotencyKey: id + ":1"},
		{StepNo: 2, Kind: "ansible-play", IdempotencyKey: id + ":2"},
	}
	if err := st.CreateWorkflow(context.Background(), id, steps); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
}

// TestHandoffStatusTextUnchanged pins that --output text equals the bare
// invocation for a seeded, still-running workflow.
func TestHandoffStatusTextUnchanged(t *testing.T) {
	st := seedStore(t)
	seedTwoStepWorkflow(t, st, "handoff:web:1")

	bare, err := run(t, "handoff", "status", "handoff:web:1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bare, "running") || !strings.Contains(bare, "tofu-apply") {
		t.Fatalf("bare = %q", bare)
	}
	explicit, err := run(t, "handoff", "status", "handoff:web:1", "--output", "text")
	if err != nil {
		t.Fatal(err)
	}
	if explicit != bare {
		t.Fatalf("--output text (%q) differs from default (%q)", explicit, bare)
	}
}

// TestHandoffStatusJSON proves the envelope shape and that the JSON facts
// match the text facts for a seeded, still-pending workflow.
func TestHandoffStatusJSON(t *testing.T) {
	st := seedStore(t)
	seedTwoStepWorkflow(t, st, "handoff:web:2")

	out, err := run(t, "handoff", "status", "handoff:web:2", "--output", "json")
	if err != nil {
		t.Fatalf("handoff status --output json: %v (%s)", err, out)
	}
	var env struct {
		SchemaVersion int `json:"schema_version"`
		Kind          string
		Data          struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Steps []struct {
				StepNo int    `json:"step_no"`
				Kind   string `json:"kind"`
				Status string `json:"status"`
				JobID  *int64 `json:"job_id"`
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if env.SchemaVersion != 1 || env.Kind != "workflow.status" {
		t.Fatalf("bad envelope header: %+v", env)
	}
	if env.Data.ID != "handoff:web:2" || env.Data.State != "running" || len(env.Data.Steps) != 2 {
		t.Fatalf("json facts do not match seeded workflow: %+v", env.Data)
	}
	if env.Data.Steps[0].Kind != "tofu-apply" || env.Data.Steps[0].Status != "pending" || env.Data.Steps[0].JobID != nil {
		t.Fatalf("step 1 facts wrong: %+v", env.Data.Steps[0])
	}
}

// TestHandoffStatusJSONNotFoundExitCode proves the JSON path exits
// wire.ExitNotFound for an unknown workflow id, matching the text path.
func TestHandoffStatusJSONNotFoundExitCode(t *testing.T) {
	seedStore(t)

	_, err := run(t, "handoff", "status", "no-such-workflow", "--output", "json")
	if err == nil {
		t.Fatal("want an error for an unknown workflow id")
	}
	if exitCodeFor(err) != 3 {
		t.Fatalf("exit code = %d, want 3 (not_found)", exitCodeFor(err))
	}

	_, textErr := run(t, "handoff", "status", "no-such-workflow")
	if textErr == nil {
		t.Fatal("want an error for an unknown workflow id (text path)")
	}
	if exitCodeFor(textErr) != 3 {
		t.Fatalf("text exit code = %d, want 3 (not_found)", exitCodeFor(textErr))
	}
}
