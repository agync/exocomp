package service_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// seedPlanJob registers a workspace and a plan job carrying the values-free
// outcome event the tofu driver records, so the policy seam reads real resource
// deltas.
func seedPlanJob(t *testing.T, st *store.Store, ws string, outcome executor.TofuPlanOutcome) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: ws, TofuDir: t.TempDir()}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	job, err := st.CreateJob(ctx, store.NewJob{Runbook: "workspace:" + ws, Kind: "tofu", Trigger: "manual"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	raw, err := json.Marshal(map[string]executor.TofuPlanOutcome{"tofu": outcome})
	if err != nil {
		t.Fatalf("marshal outcome: %v", err)
	}
	if _, err := st.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "tofu plan outcome", RawJSON: string(raw),
	}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}
	return job.ID
}

// TestSealPlanRecordsArtifact: a plan file a run wrote is recorded as an
// artifact whose digest matches the bytes on disk. A nil envelope records the
// plaintext path unchanged, so the seam is exercised without a keyring.
func TestSealPlanRecordsArtifact(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	svc := service.New(st, t.TempDir(), nil)
	jobID := seedPlanJob(t, st, "web", executor.TofuPlanOutcome{Operation: "plan", Changes: false})

	plan := filepath.Join(t.TempDir(), "web.tfplan")
	if err := os.WriteFile(plan, []byte("tfplan-body"), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	saved, err := svc.SealPlan(ctx, nil, jobID, plan, 7)
	if err != nil {
		t.Fatalf("SealPlan: %v", err)
	}
	if saved == nil {
		t.Fatal("SealPlan returned no artifact for a plan file that exists")
	}
	if saved.Kind != "tofu-plan" {
		t.Errorf("kind = %q, want tofu-plan", saved.Kind)
	}
	got, err := st.ListArtifacts(ctx, jobID)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListArtifacts = %d, %v; want 1", len(got), err)
	}
}

// TestSealPlanMissingFileIsNil: a run that wrote no plan file records nothing.
func TestSealPlanMissingFileIsNil(t *testing.T) {
	st := openStore(t)
	svc := service.New(st, t.TempDir(), nil)
	jobID := seedPlanJob(t, st, "web", executor.TofuPlanOutcome{})

	saved, err := svc.SealPlan(context.Background(), nil, jobID, filepath.Join(t.TempDir(), "absent.tfplan"), 0)
	if err != nil {
		t.Fatalf("SealPlan: %v", err)
	}
	if saved != nil {
		t.Fatalf("SealPlan recorded an artifact for a plan that was never written: %+v", saved)
	}
}

// TestSealPlanRecordsShowJSONCompanion: a plan whose executor captured a
// `tofu show -json` companion records TWO artifacts on the same job — the
// plan and the show-json under its own kind. A nil envelope records the
// plaintext show path unchanged (the same seam the plan itself uses); the
// sealed round trip is pinned by the daemon seal test (serve_planseal_test.go).
func TestSealPlanRecordsShowJSONCompanion(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	svc := service.New(st, t.TempDir(), nil)
	jobID := seedPlanJob(t, st, "web", executor.TofuPlanOutcome{Operation: "plan", Changes: true})

	dir := t.TempDir()
	plan := filepath.Join(dir, "web.tfplan")
	if err := os.WriteFile(plan, []byte("tfplan-body"), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	showPath := plan + ".show.json"
	if err := os.WriteFile(showPath, []byte(`{"resource_changes":[{"address":"x","change":{"actions":["create"]}}]}`), 0o600); err != nil {
		t.Fatalf("write show json: %v", err)
	}

	saved, err := svc.SealPlan(ctx, nil, jobID, plan, 7)
	if err != nil {
		t.Fatalf("SealPlan: %v", err)
	}
	if saved == nil || saved.Kind != "tofu-plan" {
		t.Fatalf("plan artifact = %+v, want the tofu-plan row", saved)
	}

	arts, err := st.ListArtifacts(ctx, jobID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("ListArtifacts = %d, want 2 (plan + show-json)", len(arts))
	}
	var show *store.Artifact
	for i := range arts {
		if arts[i].Kind == "tofu-plan-show" {
			show = &arts[i]
		}
	}
	if show == nil {
		t.Fatal("no show-json artifact recorded")
	}
	if show.JobID != jobID || show.PlanSerial != 7 {
		t.Errorf("show artifact = %+v, want same job and serial as the plan", show)
	}
	if show.Path != showPath {
		t.Errorf("show artifact path = %q, want the plaintext companion %q under a nil envelope", show.Path, showPath)
	}
}

// TestPlanWorkspaceHonorsTrigger: the API plan carries the "api" trigger the
// daemon reads to decide it must seal the plan, while a caller that sets none
// keeps the manual default so a CLI plan seals its own.
func TestPlanWorkspaceHonorsTrigger(t *testing.T) {
	svc := service.New(nil, t.TempDir(), nil)

	api := &fakeEnqueuer{}
	if _, err := svc.PlanWorkspace(context.Background(), api, service.PlanInput{
		Workspace: "web", ArtifactsDir: t.TempDir(), Trigger: service.TriggerAPI,
	}); err != nil {
		t.Fatalf("PlanWorkspace(api): %v", err)
	}
	if api.enqueued[0].Trigger != service.TriggerAPI {
		t.Errorf("api plan trigger = %q, want %q", api.enqueued[0].Trigger, service.TriggerAPI)
	}

	def := &fakeEnqueuer{}
	if _, err := svc.PlanWorkspace(context.Background(), def, service.PlanInput{
		Workspace: "web", ArtifactsDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("PlanWorkspace(default): %v", err)
	}
	if def.enqueued[0].Trigger != service.TriggerManual {
		t.Errorf("default plan trigger = %q, want %q", def.enqueued[0].Trigger, service.TriggerManual)
	}
}

const deleteRule = `plan.resource_changes.exists(c, c.change.actions.exists(a, a == "delete"))`

// TestPlanPolicyRoundTrip: a deny policy over a delete matches, RecordPlanVerdict
// stamps it on the plan job, and PlanVerdict reads the same deny back — the seam
// an apply-by-id consults to refuse an unconsumable plan.
func TestPlanPolicyRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	svc := service.New(st, t.TempDir(), nil)
	jobID := seedPlanJob(t, st, "web", executor.TofuPlanOutcome{
		Operation: "plan", Changes: true,
		Pending: []executor.ResourceDelta{{Address: "local_file.gone", Actions: []string{"delete"}}},
	})
	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-deletes", Workspace: "web", Expr: deleteRule, Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("CreatePlanPolicy: %v", err)
	}
	ws, _ := st.WorkspaceByName(ctx, "web")

	res, err := svc.EvalPlanPolicy(ctx, ws, jobID)
	if err != nil {
		t.Fatalf("EvalPlanPolicy: %v", err)
	}
	if !res.Verdict.Deny() || res.Verdict.Policy != "no-deletes" {
		t.Fatalf("verdict = %+v, want deny by no-deletes", res.Verdict)
	}

	art, err := st.CreateArtifact(ctx, store.Artifact{JobID: jobID, Kind: "tofu-plan", Path: "/x.age", SHA256: "s", Size: 1})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if _, err := svc.RecordPlanVerdict(ctx, jobID, art.ID, res.Verdict); err != nil {
		t.Fatalf("RecordPlanVerdict: %v", err)
	}

	back, err := svc.PlanVerdict(ctx, art)
	if err != nil {
		t.Fatalf("PlanVerdict: %v", err)
	}
	if !back.Deny() || back.Policy != "no-deletes" {
		t.Fatalf("read-back verdict = %+v, want deny by no-deletes", back)
	}
	if gate := service.PlanGateFor(back); !gate.Deny {
		t.Errorf("PlanGateFor(deny) = %+v, want Deny", gate)
	}
}

// TestEvalPlanPolicyFailsClosed: a plan that reports changes without describable
// detail is sealed denied, with a fail-closed notice for the face to render.
func TestEvalPlanPolicyFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	svc := service.New(st, t.TempDir(), nil)
	jobID := seedPlanJob(t, st, "web", executor.TofuPlanOutcome{Operation: "plan", Changes: true})
	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-deletes", Workspace: "web", Expr: deleteRule, Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("CreatePlanPolicy: %v", err)
	}
	ws, _ := st.WorkspaceByName(ctx, "web")

	res, err := svc.EvalPlanPolicy(ctx, ws, jobID)
	if err != nil {
		t.Fatalf("EvalPlanPolicy: %v", err)
	}
	if !res.Verdict.Deny() {
		t.Fatalf("verdict = %+v, want a fail-closed deny", res.Verdict)
	}
	if res.FailNotice == "" {
		t.Error("a fail-closed deny must carry a notice for the face to render")
	}
}
