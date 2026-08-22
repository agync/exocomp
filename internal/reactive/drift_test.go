package reactive_test

import (
	"context"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// planJob writes the row a finished scheduled plan leaves behind, including
// the plan-outcome event the tofu driver attaches when -detailed-exitcode
// reported changes.
func (h *harness) planJob(t *testing.T, workspace string, changes bool) store.Job {
	t.Helper()
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: reactive.WorkspaceRunbook(workspace),
		Kind:    executor.KindTofu,
		Trigger: "schedule",
		Args:    store.JobArgs{Argv: executor.TofuArgv("plan")},
	})
	if err != nil {
		t.Fatalf("create plan job: %v", err)
	}
	raw := `{"tofu":{"operation":"plan","changes":false}}`
	if changes {
		raw = `{"tofu":{"operation":"plan","changes":true}}`
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "plan finished", RawJSON: raw,
	}); err != nil {
		t.Fatalf("append plan outcome: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("start plan job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("finish plan job: %v", err)
	}
	return job
}

// TestDriftBecomesAnEventThatCanGateAnApply is the loop the tofu-plan schedule
// kind exists for: a plan finds changes, that becomes an event in the same
// pipeline, and a rule turns it into an approval-gated apply.
func TestDriftBecomesAnEventThatCanGateAnApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	if _, err := h.store.CreateWorkspace(ctx, store.Workspace{
		Name: "prod", TofuDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	h.addRule(t, store.NewRule{
		Name:       "apply-on-drift",
		CELExpr:    `event.name == "TofuDrift" && event.labels["workspace"] == "prod"`,
		ActionKind: store.ActionTofuApply,
		Action:     store.RuleAction{Workspace: "prod"},
	})

	planned := h.planJob(t, "prod", true)

	raised, err := h.engine.SweepDrift(ctx)
	if err != nil {
		t.Fatalf("sweep drift: %v", err)
	}
	if raised != 1 {
		t.Fatalf("raised %d drift events, want 1", raised)
	}

	event := lastEvent(t, h.store)
	if event.Source != reactive.SourceDrift || event.Outcome != store.OutcomeGated {
		t.Fatalf("drift event = %+v, want a gated drift event", event)
	}
	if event.MatchedRule != "apply-on-drift" {
		t.Fatalf("matched rule = %q", event.MatchedRule)
	}
	if !strings.Contains(event.DedupKey, "prod") {
		t.Fatalf("dedup key = %q, want it to name the workspace", event.DedupKey)
	}

	job, err := h.store.GetJob(ctx, *event.JobID)
	if err != nil {
		t.Fatalf("get gated apply: %v", err)
	}
	if job.State != store.StateAwaitingApproval {
		t.Fatalf("the apply is %s, want awaiting_approval", job.State)
	}

	// The sweep is idempotent: the event table is its own checkpoint, so a
	// second run raises nothing.
	again, err := h.engine.SweepDrift(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("second sweep raised %d events for job %d", again, planned.ID)
	}
}

func TestAPlanWithNoChangesRaisesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.planJob(t, "prod", false)

	raised, err := h.engine.SweepDrift(ctx)
	if err != nil {
		t.Fatalf("sweep drift: %v", err)
	}
	if raised != 0 {
		t.Fatalf("a clean plan raised %d events", raised)
	}
}

// TestWorkspaceRunbookPrefixMatchesTheCLI pins the one string the daemon reads
// a tofu job's workspace back out of. `workspace plan|apply` writes it in
// cmd/exocomp/workspace.go; a drift plan that spelled it differently would
// enqueue a job the driver cannot build.
func TestWorkspaceRunbookPrefixMatchesTheCLI(t *testing.T) {
	t.Parallel()

	if reactive.WorkspaceRunbookPrefix != "workspace:" {
		t.Fatalf("prefix = %q, want %q", reactive.WorkspaceRunbookPrefix, "workspace:")
	}
	if got := reactive.WorkspaceRunbook("prod"); got != "workspace:prod" {
		t.Fatalf("runbook = %q", got)
	}
	name, ok := reactive.WorkspaceOf("workspace:prod")
	if !ok || name != "prod" {
		t.Fatalf("WorkspaceOf = %q, %v", name, ok)
	}
	if _, ok := reactive.WorkspaceOf("rule:restart"); ok {
		t.Fatal("a rule runbook was read as a workspace")
	}
	if _, ok := reactive.WorkspaceOf("workspace:"); ok {
		t.Fatal("an empty workspace name was accepted")
	}
	if reactive.PlanArtifactKind != "tofu-plan" {
		t.Fatalf("plan artifact kind = %q", reactive.PlanArtifactKind)
	}
}
