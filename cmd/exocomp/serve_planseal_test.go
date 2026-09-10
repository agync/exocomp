package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// TestDaemonSealsAPIPlan walks the API-initiated plan seal the daemon owns: an
// "api"-triggered plan job whose run wrote an -out file gets that file sealed
// into a consumable artifact, its policy verdict recorded, the plaintext -out
// removed, and the sealed plan opens back to its original bytes — the exact
// round trip an apply-by-id relies on.
func TestDaemonSealsAPIPlan(t *testing.T) {
	dir, _ := initForKit(t)
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	defer func() { _ = env.Close() }()
	dirs, err := tofuDirs(dir)
	if err != nil {
		t.Fatalf("tofuDirs: %v", err)
	}
	st, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "web", TofuDir: t.TempDir()}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	job, err := st.CreateJob(ctx, store.NewJob{
		Runbook: workspaceRunbookPrefix + "web", Kind: "tofu", Trigger: service.TriggerAPI,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	outcome := executor.TofuPlanOutcome{
		Operation: "plan", Changes: true,
		Pending: []executor.ResourceDelta{{Address: "local_file.gone", Actions: []string{"delete"}}},
	}
	raw, _ := json.Marshal(map[string]executor.TofuPlanOutcome{"tofu": outcome})
	if _, err := st.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "outcome", RawJSON: string(raw),
	}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}
	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-deletes", Workspace: "web", Expr: deleteRuleExpr2, Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("CreatePlanPolicy: %v", err)
	}

	planPath := filepath.Join(dirs.artifacts, "web-1.tfplan")
	if err := os.WriteFile(planPath, []byte(planBody), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	showJSON := `{"resource_changes":[{"address":"local_file.gone","change":{"actions":["delete"]}}]}`
	if err := os.WriteFile(planPath+".show.json", []byte(showJSON), 0o600); err != nil {
		t.Fatalf("write show json: %v", err)
	}

	ws, _ := st.WorkspaceByName(ctx, "web")
	d := workspaceDriver{store: st, env: env, dirs: dirs}
	d.sealAPIPlan(ctx, job, ws, executor.TofuArgv("plan", "-out="+planPath), &collectSink{})

	arts, err := st.ListArtifacts(ctx, job.ID)
	if err != nil || len(arts) != 2 {
		t.Fatalf("ListArtifacts = %d, %v; want 2 (sealed plan + sealed show-json)", len(arts), err)
	}
	saved := arts[0]
	if !strings.HasSuffix(saved.Path, secrets.SealedSuffix) {
		t.Fatalf("artifact %s is not sealed", saved.Path)
	}
	if _, err := os.Stat(planPath); !os.IsNotExist(err) {
		t.Errorf("the plaintext -out survived the seal (stat err = %v)", err)
	}

	// The show-json companion is sealed under its own kind and opens back to
	// the JSON the web renders the plan's diff from.
	var show *store.Artifact
	for i := range arts {
		if arts[i].Kind == "tofu-plan-show" {
			show = &arts[i]
		}
	}
	if show == nil {
		t.Fatal("no sealed show-json artifact recorded")
	}
	if !strings.HasSuffix(show.Path, secrets.SealedSuffix) {
		t.Fatalf("show artifact %s is not sealed", show.Path)
	}
	showBody, err := env.OpenSealedFile(show.Path)
	if err != nil {
		t.Fatalf("open sealed show json: %v", err)
	}
	if string(showBody) != showJSON {
		t.Errorf("decrypted show json = %q, want the captured JSON", showBody)
	}
	if _, err := os.Stat(planPath + ".show.json"); !os.IsNotExist(err) {
		t.Errorf("the plaintext show json survived the seal (stat err = %v)", err)
	}

	v, err := planVerdict(ctx, st, saved)
	if err != nil {
		t.Fatalf("planVerdict: %v", err)
	}
	if !v.Deny() || v.Policy != "no-deletes" {
		t.Fatalf("recorded verdict = %+v, want deny by no-deletes", v)
	}

	// apply-by-id opens the sealed plan back to the run-private plaintext.
	readable, release, err := openPlanForRun(env, dirs.private, saved.Path)
	if err != nil {
		t.Fatalf("openPlanForRun: %v", err)
	}
	defer release()
	body, err := os.ReadFile(readable)
	if err != nil {
		t.Fatalf("read decrypted plan: %v", err)
	}
	if string(body) != planBody {
		t.Errorf("decrypted plan = %q, want the original", body)
	}
}

// TestApplyPlanArgHelpers pins the argv parsing the daemon uses to find and
// rewrite the saved-plan file an apply reads.
func TestApplyPlanArgHelpers(t *testing.T) {
	sealed := executor.TofuArgv("apply", "/data/plan.tfplan.age")
	plan, ok := applyPlanArg(sealed)
	if !ok || plan != "/data/plan.tfplan.age" {
		t.Fatalf("applyPlanArg(%v) = %q, %v", sealed, plan, ok)
	}
	rewritten := setApplyPlanArg(sealed, "/private/saved.tfplan")
	if rewritten[2] != "/private/saved.tfplan" {
		t.Errorf("setApplyPlanArg did not rewrite: %v", rewritten)
	}
	if sealed[2] != "/data/plan.tfplan.age" {
		t.Errorf("setApplyPlanArg mutated the caller's slice: %v", sealed)
	}

	// A fresh apply names no plan file.
	if _, ok := applyPlanArg(executor.TofuArgv("apply", "-auto-approve")); ok {
		t.Error("applyPlanArg matched a bare -auto-approve apply")
	}

	if got := planOutArg(executor.TofuArgv("plan", "-out=/data/web-9.tfplan")); got != "/data/web-9.tfplan" {
		t.Errorf("planOutArg = %q, want /data/web-9.tfplan", got)
	}
	if got := planOutArg(executor.TofuArgv("plan")); got != "" {
		t.Errorf("planOutArg with no -out = %q, want empty", got)
	}
}
