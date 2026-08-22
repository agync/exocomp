package main

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/store"
)

// seedPlan registers a workspace and a completed plan job with a sealed plan
// artifact, returning the artifact id an apply would name. No real plan file is
// needed: the deny and require_approval paths decide on the recorded verdict
// before any plan is opened.
func seedPlan(t *testing.T, ws string) (st *store.Store, artifactID int64) {
	t.Helper()
	var err error
	st, err = openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: ws, TofuDir: t.TempDir()}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	job, err := st.CreateJob(ctx, store.NewJob{
		Runbook: workspaceRunbookPrefix + ws, Kind: "tofu", Trigger: "manual",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	art, err := st.CreateArtifact(ctx, store.Artifact{
		JobID: job.ID, Kind: planArtifactKind,
		Path: t.TempDir() + "/plan.tfplan.age", SHA256: "deadbeef", Size: 1,
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	return st, art.ID
}

// TestPlanVerdictRoundTrip proves a recorded verdict reads back keyed to its
// artifact, and that an unrelated artifact on the same job reads clean.
func TestPlanVerdictRoundTrip(t *testing.T) {
	initForKit(t)
	st, artifactID := seedPlan(t, "homelab")
	ctx := context.Background()

	art, err := st.GetArtifact(ctx, artifactID)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	if err := recordPlanVerdict(ctx, st, &strings.Builder{}, art.JobID, art.ID,
		policy.Verdict{Policy: "no-deletes", Action: store.PolicyDeny}); err != nil {
		t.Fatalf("recordPlanVerdict: %v", err)
	}

	v, err := planVerdict(ctx, st, art)
	if err != nil {
		t.Fatalf("planVerdict: %v", err)
	}
	if !v.Deny() || v.Policy != "no-deletes" {
		t.Fatalf("verdict = %+v, want deny by no-deletes", v)
	}

	// An artifact the verdict was not keyed to must read clean, so one plan's
	// verdict cannot make an unrelated plan unconsumable.
	other := store.Artifact{ID: art.ID + 999, JobID: art.JobID}
	if v, err := planVerdict(ctx, st, other); err != nil || v.Matched() {
		t.Fatalf("unrelated artifact verdict = %+v (err %v), want clean", v, err)
	}
}

// TestApplyRefusesDeniedPlan is the core gate: a sealed plan a deny policy
// matched is unconsumable, so `workspace apply --plan <id>` refuses naming the
// policy, and enqueues nothing.
func TestApplyRefusesDeniedPlan(t *testing.T) {
	initForKit(t)
	st, artifactID := seedPlan(t, "homelab")
	ctx := context.Background()

	art, _ := st.GetArtifact(ctx, artifactID)
	if err := recordPlanVerdict(ctx, st, &strings.Builder{}, art.JobID, art.ID,
		policy.Verdict{Policy: "no-deletes", Action: store.PolicyDeny}); err != nil {
		t.Fatalf("recordPlanVerdict: %v", err)
	}

	out, err := runCobra(t, newWorkspaceCmd, "apply", "homelab", "--plan", strconv.FormatInt(artifactID, 10))
	if err == nil {
		t.Fatalf("apply of a denied plan succeeded; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no-deletes") || !strings.Contains(err.Error(), "unconsumable") {
		t.Fatalf("refusal did not name the policy: %v", err)
	}

	// Nothing was enqueued: the only job is the plan job that produced the
	// artifact.
	jobs, _ := st.ListJobs(ctx, store.JobFilter{Runbook: workspaceRunbookPrefix + "homelab"})
	if len(jobs) != 1 {
		t.Fatalf("a denied apply enqueued a job: %d workspace jobs exist", len(jobs))
	}
}

// TestApplyGatesRequireApprovalPlan proves a require_approval plan's apply is
// routed through the existing gate: the job lands in awaiting_approval and an
// approval is created, rather than the apply running.
func TestApplyGatesRequireApprovalPlan(t *testing.T) {
	initForKit(t)
	st, artifactID := seedPlan(t, "homelab")
	ctx := context.Background()

	art, _ := st.GetArtifact(ctx, artifactID)
	if err := recordPlanVerdict(ctx, st, &strings.Builder{}, art.JobID, art.ID,
		policy.Verdict{Policy: "gate-changes", Action: store.PolicyRequireApproval}); err != nil {
		t.Fatalf("recordPlanVerdict: %v", err)
	}

	out, err := runCobra(t, newWorkspaceCmd, "apply", "homelab", "--plan", strconv.FormatInt(artifactID, 10))
	if err != nil {
		t.Fatalf("gated apply returned an error: %v (%s)", err, out)
	}
	if !strings.Contains(out, "gate-changes") || !strings.Contains(out, "awaiting_approval") {
		t.Fatalf("gated apply output did not report the gate:\n%s", out)
	}

	pending, err := st.ListApprovals(ctx, true)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("gated apply created %d pending approvals, want 1", len(pending))
	}
	gated, err := st.GetJob(ctx, pending[0].JobID)
	if err != nil {
		t.Fatalf("get gated job: %v", err)
	}
	if gated.State != store.StateAwaitingApproval {
		t.Fatalf("gated job state = %s, want awaiting_approval", gated.State)
	}
	if pending[0].PlanArtifact == nil || *pending[0].PlanArtifact != artifactID {
		t.Fatalf("approval does not name the plan artifact: %+v", pending[0])
	}
}
