package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// fakeEnqueuer records the spec each call received and hands back a canned job,
// so a verb's spec assembly and its enqueue/gate choice can be asserted without
// a queue.
type fakeEnqueuer struct {
	enqueued []queue.Spec
	gated    []queue.Spec
	nextID   int64
	err      error
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, spec queue.Spec) (store.Job, error) {
	if f.err != nil {
		return store.Job{}, f.err
	}
	f.enqueued = append(f.enqueued, spec)
	f.nextID++
	return store.Job{ID: f.nextID, Runbook: spec.Runbook, State: store.StateSucceeded}, nil
}

func (f *fakeEnqueuer) EnqueueGated(_ context.Context, spec queue.Spec) (store.Job, error) {
	if f.err != nil {
		return store.Job{}, f.err
	}
	f.gated = append(f.gated, spec)
	f.nextID++
	return store.Job{ID: f.nextID, Runbook: spec.Runbook, State: store.StateAwaitingApproval}, nil
}

// recordingEnqueuer delegates to a real queue — so an approval's FK to a real
// job row is satisfied — while recording which method each spec went through.
type recordingEnqueuer struct {
	q        *queue.Queue
	enqueued []queue.Spec
	gated    []queue.Spec
}

func (r *recordingEnqueuer) Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error) {
	r.enqueued = append(r.enqueued, spec)
	return r.q.Enqueue(ctx, spec)
}

func (r *recordingEnqueuer) EnqueueGated(ctx context.Context, spec queue.Spec) (store.Job, error) {
	r.gated = append(r.gated, spec)
	return r.q.EnqueueGated(ctx, spec)
}

// fixedDeriver is a deterministic stand-in for the secrets envelope, enough for
// the approval token an approval row stores.
type fixedDeriver struct{}

func (fixedDeriver) Derive(purpose, id string) []byte {
	return []byte("derived-" + purpose + "-" + id + "-key-material")
}

func fixedClock() func() time.Time {
	t := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func TestRunPlaybookAssemblesAnsibleSpec(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())

	res, err := svc.RunPlaybook(context.Background(), enq, service.PlaybookInput{
		PlaybookPath:        "/abs/site.yml",
		InventoryPath:       "/abs/hosts.ini",
		InventoryName:       "prod",
		VaultProject:        "vault-a",
		RunSecrets:          "aws",
		Passthrough:         []string{"--limit", "hostA"},
		ExtraVars:           map[string]string{"proof": "xyz"},
		TolerateUnreachable: true,
		IdempotencyKey:      "idem-1",
		Timeout:             30 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	if len(enq.enqueued) != 1 || len(enq.gated) != 0 {
		t.Fatalf("want one plain enqueue and no gate, got %d/%d", len(enq.enqueued), len(enq.gated))
	}
	spec := enq.enqueued[0]
	if spec.Runbook != "playbook" || spec.Kind != executor.KindAnsible || spec.Trigger != "manual" {
		t.Errorf("spec header = %q/%q/%q", spec.Runbook, spec.Kind, spec.Trigger)
	}
	argv := strings.Join(spec.Argv, " ")
	if !strings.Contains(argv, "/abs/site.yml") || !strings.Contains(argv, "/abs/hosts.ini") ||
		!strings.Contains(argv, "--limit hostA") {
		t.Errorf("argv did not carry the playbook, inventory, and passthrough: %v", spec.Argv)
	}
	if spec.VaultProject != "vault-a" || spec.RunSecretBinding != "aws" || spec.Inventory != "prod" {
		t.Errorf("secret/inventory bindings lost: %+v", spec)
	}
	if spec.ExtraVars["proof"] != "xyz" || !spec.TolerateUnreachable ||
		spec.IdempotencyKey != "idem-1" || spec.Timeout != 30*time.Second {
		t.Errorf("run options lost: %+v", spec)
	}
	if res.Job.ID != 1 {
		t.Errorf("result job id = %d, want 1", res.Job.ID)
	}
}

// TestRunPlaybookCarriesSourceDir proves a --source run's checkout root and its
// read-only posture reach the enqueued spec as Dir/DirReadOnly, so the executor
// runs ansible from that tree (loading its ./ansible.cfg) and grants it read,
// not write. A raw-path run names no Dir, leaving the working directory to the
// executor's per-job scratch — the pre-fix behavior it must not regress.
func TestRunPlaybookCarriesSourceDir(t *testing.T) {
	svc := service.New(nil, t.TempDir(), fixedClock())

	t.Run("source run carries the checkout root read-only", func(t *testing.T) {
		enq := &fakeEnqueuer{}
		if _, err := svc.RunPlaybook(context.Background(), enq, service.PlaybookInput{
			PlaybookPath: "/checkout/site.yml",
			Dir:          "/checkout",
			DirReadOnly:  true,
		}); err != nil {
			t.Fatalf("RunPlaybook: %v", err)
		}
		spec := enq.enqueued[0]
		if spec.Dir != "/checkout" || !spec.DirReadOnly {
			t.Errorf("Dir/DirReadOnly not carried: Dir=%q DirReadOnly=%v", spec.Dir, spec.DirReadOnly)
		}
	})

	t.Run("raw-path run names no working directory", func(t *testing.T) {
		enq := &fakeEnqueuer{}
		if _, err := svc.RunPlaybook(context.Background(), enq, service.PlaybookInput{
			PlaybookPath: "/abs/site.yml",
		}); err != nil {
			t.Fatalf("RunPlaybook: %v", err)
		}
		spec := enq.enqueued[0]
		if spec.Dir != "" || spec.DirReadOnly {
			t.Errorf("a raw-path run set a working directory: Dir=%q DirReadOnly=%v", spec.Dir, spec.DirReadOnly)
		}
	})
}

func TestRunPlaybookPropagatesEnqueueError(t *testing.T) {
	enq := &fakeEnqueuer{err: errors.New("boom")}
	svc := service.New(nil, t.TempDir(), fixedClock())
	if _, err := svc.RunPlaybook(context.Background(), enq, service.PlaybookInput{}); err == nil {
		t.Fatal("want the enqueue error surfaced")
	}
}

func TestPlanWorkspaceAssemblesTofuPlanSpec(t *testing.T) {
	enq := &fakeEnqueuer{}
	artifacts := t.TempDir()
	svc := service.New(nil, t.TempDir(), fixedClock())

	res, err := svc.PlanWorkspace(context.Background(), enq, service.PlanInput{
		Workspace:      "web",
		TofuDir:        "/roots/web",
		ArtifactsDir:   artifacts,
		RunSecrets:     "provider",
		IdempotencyKey: "idem-2",
		Timeout:        time.Minute,
	})
	if err != nil {
		t.Fatalf("PlanWorkspace: %v", err)
	}
	spec := enq.enqueued[0]
	if spec.Runbook != "workspace:web" || spec.Kind != executor.KindTofu {
		t.Errorf("spec header = %q/%q", spec.Runbook, spec.Kind)
	}
	if spec.ConcurrencyKey != "tofu:web" {
		t.Errorf("concurrency key = %q, want tofu:web", spec.ConcurrencyKey)
	}
	if spec.Dir != "/roots/web" || spec.RunSecretBinding != "provider" || spec.IdempotencyKey != "idem-2" {
		t.Errorf("plan options lost: %+v", spec)
	}
	argv := strings.Join(spec.Argv, " ")
	if !strings.Contains(argv, "plan") || !strings.Contains(argv, "-out="+artifacts) {
		t.Errorf("argv missing plan -out under the artifacts dir: %v", spec.Argv)
	}
	if filepath.Dir(res.PlanPath) != artifacts || !strings.HasSuffix(res.PlanPath, ".tfplan") {
		t.Errorf("plan path = %q, want a .tfplan under %q", res.PlanPath, artifacts)
	}
	// The verb reports the same path the run writes to.
	if !strings.Contains(argv, res.PlanPath) {
		t.Errorf("argv -out and reported plan path disagree: %v vs %q", spec.Argv, res.PlanPath)
	}
}

func TestApplyWorkspaceFreshEnqueuesPlain(t *testing.T) {
	enq := &fakeEnqueuer{}
	// A real store: the verb reads the workspace's plan policies before it lets a
	// fresh apply through, and this workspace has none.
	svc := service.New(openStore(t), t.TempDir(), fixedClock())
	built := false

	res, err := svc.ApplyWorkspace(context.Background(), enq, fixedDeriver{}, service.ApplyInput{
		Workspace: "web",
		TofuDir:   "/roots/web",
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		built = true
		return executor.TofuArgv("apply", "-auto-approve"), nil, func() {}, nil
	})
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	if !built {
		t.Fatal("a plain apply must build its argv")
	}
	if res.Gated || res.Run == nil {
		t.Fatalf("want a plain run, got gated=%v run=%v", res.Gated, res.Run)
	}
	if len(enq.gated) != 0 || len(enq.enqueued) != 1 {
		t.Fatalf("a fresh apply must not gate: %d gated", len(enq.gated))
	}
	if !strings.Contains(strings.Join(enq.enqueued[0].Argv, " "), "-auto-approve") {
		t.Errorf("argv = %v", enq.enqueued[0].Argv)
	}
}

func TestApplyWorkspaceDenyRefusesWithoutEnqueue(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())
	built := false

	_, err := svc.ApplyWorkspace(context.Background(), enq, fixedDeriver{}, service.ApplyInput{
		Workspace: "web",
		SavedPlan: &store.Artifact{ID: 7},
		Gate:      service.PlanGate{Deny: true, Policy: "no-deletes"},
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		built = true
		return nil, nil, func() {}, nil
	})

	var denied *service.PlanDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want PlanDeniedError, got %v", err)
	}
	if denied.PlanArtifactID != 7 || denied.Policy != "no-deletes" {
		t.Errorf("denied = %+v", denied)
	}
	if built {
		t.Error("a denied plan must not build its argv or open its plaintext")
	}
	if len(enq.enqueued)+len(enq.gated) != 0 {
		t.Error("a denied plan must not enqueue anything")
	}
}

// TestApplyWorkspaceRequireApprovalGates is the guardrail: a require_approval
// verdict must EnqueueGated (never Enqueue), create a pending approval, and
// never open the plan's plaintext.
func TestApplyWorkspaceRequireApprovalGates(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	enq := &recordingEnqueuer{q: q}
	svc := service.New(st, t.TempDir(), fixedClock())
	built := false

	// The apply's approval FKs a real plan job and its saved artifact.
	planJob, err := q.Enqueue(ctx, queue.Spec{
		Runbook: "workspace:web", Kind: executor.KindTofu, Trigger: "manual",
		Argv: executor.TofuArgv("plan", "-out=/data/artifacts/web-9.tfplan"),
	})
	if err != nil {
		t.Fatalf("seed plan job: %v", err)
	}
	saved, err := st.CreateArtifact(ctx, store.Artifact{
		JobID: planJob.ID, Kind: "tofu-plan",
		Path: "/data/artifacts/web-9.tfplan.age", SHA256: "deadbeef", Size: 10,
	})
	if err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	res, err := svc.ApplyWorkspace(ctx, enq, fixedDeriver{}, service.ApplyInput{
		Workspace:       "web",
		WorkspaceSerial: 4,
		TofuDir:         "/roots/web",
		SavedPlan:       &saved,
		Gate:            service.PlanGate{RequireApproval: true, Policy: "prod-guard"},
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		built = true
		return nil, nil, func() {}, nil
	})
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	if built {
		t.Error("a gated apply must not open the plan plaintext")
	}
	if len(enq.enqueued) != 0 || len(enq.gated) != 1 {
		t.Fatalf("a require_approval apply must EnqueueGated, not Enqueue: %d plain / %d gated",
			len(enq.enqueued), len(enq.gated))
	}
	// The gated spec applies the sealed plan directly; the daemon opens it on release.
	if !strings.Contains(strings.Join(enq.gated[0].Argv, " "), saved.Path) {
		t.Errorf("gated argv = %v", enq.gated[0].Argv)
	}
	// The gated job must land where no worker can claim it.
	gatedJob, err := st.GetJob(context.Background(), res.Approval.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if gatedJob.State != store.StateAwaitingApproval {
		t.Errorf("gated job state = %q, want awaiting_approval", gatedJob.State)
	}
	if !res.Gated || res.Run != nil {
		t.Fatalf("want a gated result, got %+v", res)
	}
	a := res.Approval
	if a.PlanArtifactID != saved.ID || a.Policy != "prod-guard" || a.State != store.ApprovalPending {
		t.Errorf("approval summary = %+v", a)
	}
	// The approval row was created and points at the gated job.
	stored, err := st.GetApproval(context.Background(), a.ApprovalID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if stored.JobID != a.JobID || stored.Workspace != "web" || stored.PlanSerial != 4 {
		t.Errorf("stored approval = %+v", stored)
	}
	if stored.TokenHash == "" {
		t.Error("the approval must store a token hash; the token itself is never persisted")
	}
	if !stored.ExpiresAt.After(stored.RequestedAt) {
		t.Error("the approval must carry a TTL window")
	}
}

// TestApplyWorkspaceGatedIdempotencyReusesApproval is the resubmission case: a
// client that retries a gated apply under the same idempotency key (a timeout,
// a double-click) must land on the SAME approval, not a second pending row for
// a job that already has one. EnqueueGated already dedups the job; gatedApply
// must dedup the approval it points at, or the approval table's job_id UNIQUE
// constraint turns the retry into an error instead of a no-op.
func TestApplyWorkspaceGatedIdempotencyReusesApproval(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	enq := &recordingEnqueuer{q: q}
	svc := service.New(st, t.TempDir(), fixedClock())

	planJob, err := q.Enqueue(ctx, queue.Spec{
		Runbook: "workspace:web", Kind: executor.KindTofu, Trigger: "manual",
		Argv: executor.TofuArgv("plan", "-out=/data/artifacts/web-9.tfplan"),
	})
	if err != nil {
		t.Fatalf("seed plan job: %v", err)
	}
	saved, err := st.CreateArtifact(ctx, store.Artifact{
		JobID: planJob.ID, Kind: "tofu-plan",
		Path: "/data/artifacts/web-9.tfplan.age", SHA256: "deadbeef", Size: 10,
	})
	if err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	in := service.ApplyInput{
		Workspace:       "web",
		WorkspaceSerial: 4,
		TofuDir:         "/roots/web",
		IdempotencyKey:  "resubmit-1",
		SavedPlan:       &saved,
		Gate:            service.PlanGate{RequireApproval: true, Policy: "prod-guard"},
	}
	buildArgv := func(context.Context) ([]string, *store.Artifact, func(), error) {
		return nil, nil, func() {}, nil
	}

	first, err := svc.ApplyWorkspace(ctx, enq, fixedDeriver{}, in, buildArgv)
	if err != nil {
		t.Fatalf("first ApplyWorkspace: %v", err)
	}
	if !first.Gated {
		t.Fatalf("first apply must gate: %+v", first)
	}

	second, err := svc.ApplyWorkspace(ctx, enq, fixedDeriver{}, in, buildArgv)
	if err != nil {
		t.Fatalf("resubmitted ApplyWorkspace under the same idempotency key errored: %v", err)
	}
	if !second.Gated {
		t.Fatalf("resubmit must gate too: %+v", second)
	}
	if second.Approval.JobID != first.Approval.JobID {
		t.Errorf("resubmit landed on job %d, want the original job %d",
			second.Approval.JobID, first.Approval.JobID)
	}
	if second.Approval.ApprovalID != first.Approval.ApprovalID {
		t.Errorf("resubmit minted approval %d, want the original approval %d reused",
			second.Approval.ApprovalID, first.Approval.ApprovalID)
	}

	approvals, err := st.ListApprovals(ctx, false)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(approvals) != 1 {
		t.Fatalf("approval rows = %d, want exactly 1", len(approvals))
	}

	eng, err := reactive.New(reactive.Options{Store: st, Queue: &engineGate{}, Deriver: fixedDeriver{}})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	token, _, err := reactive.ApprovalToken(fixedDeriver{}, first.Approval.JobID)
	if err != nil {
		t.Fatalf("approval token: %v", err)
	}
	if _, err := svc.DecideApproval(ctx, eng, service.DecideInput{
		ApprovalID: first.Approval.ApprovalID,
		Decision:   reactive.DecisionApprove,
		Token:      token,
		Via:        "cli",
	}); err != nil {
		t.Fatalf("deciding the reused approval failed: %v", err)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// engineGate records what an approval decision drove into the queue, so the
// fail-closed path can be observed without a running queue.
type engineGate struct {
	released  []int64
	withdrawn []int64
}

func (g *engineGate) Enqueue(context.Context, queue.Spec) (store.Job, error) {
	return store.Job{}, nil
}
func (g *engineGate) EnqueueGated(context.Context, queue.Spec) (store.Job, error) {
	return store.Job{}, nil
}
func (g *engineGate) Release(_ context.Context, jobID int64) error {
	g.released = append(g.released, jobID)
	return nil
}
func (g *engineGate) Withdraw(_ context.Context, jobID int64) error {
	g.withdrawn = append(g.withdrawn, jobID)
	return nil
}

// decideFixture seeds one pending approval over a real job and returns the engine
// and the derived token, so DecideApproval can be exercised end to end.
func decideFixture(t *testing.T, gate *engineGate) (*service.Service, *reactive.Engine, store.Approval, string) {
	t.Helper()
	ctx := context.Background()
	st := openStore(t)
	deriver := fixedDeriver{}

	eng, err := reactive.New(reactive.Options{Store: st, Queue: gate, Deriver: deriver})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	job, err := q.EnqueueGated(ctx, queue.Spec{
		Runbook: "workspace:web", Kind: executor.KindTofu, Trigger: "manual",
		Argv: executor.TofuArgv("apply", "/data/artifacts/web-1.tfplan"),
	})
	if err != nil {
		t.Fatalf("enqueue gated: %v", err)
	}
	token, hash, err := reactive.ApprovalToken(deriver, job.ID)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	now := time.Now().UTC()
	approval, err := st.CreateApproval(ctx, store.Approval{
		JobID:       job.ID,
		Workspace:   "web",
		RequestedAt: now,
		ExpiresAt:   now.Add(30 * time.Minute),
		TokenHash:   hash,
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	return service.New(st, t.TempDir(), fixedClock()), eng, approval, token
}

func TestDecideApprovalApproveReleases(t *testing.T) {
	gate := &engineGate{}
	svc, eng, approval, token := decideFixture(t, gate)

	res, err := svc.DecideApproval(context.Background(), eng, service.DecideInput{
		ApprovalID: approval.ID,
		Decision:   reactive.DecisionApprove,
		Token:      token,
		Via:        "cli",
	})
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if res.Decision.Outcome != reactive.DecisionOutcomeApproved {
		t.Fatalf("outcome = %q, want approved", res.Decision.Outcome)
	}
	if len(gate.released) != 1 || gate.released[0] != approval.JobID {
		t.Errorf("approve must release the gated job, released=%v", gate.released)
	}
}

// TestDecideApprovalFailsClosedOnBadToken proves the verb adds no bypass: a
// forged token is refused and nothing is released.
func TestDecideApprovalFailsClosedOnBadToken(t *testing.T) {
	gate := &engineGate{}
	svc, eng, approval, _ := decideFixture(t, gate)

	_, err := svc.DecideApproval(context.Background(), eng, service.DecideInput{
		ApprovalID: approval.ID,
		Decision:   reactive.DecisionApprove,
		Token:      "forged",
		Via:        "cli",
	})
	if err == nil {
		t.Fatal("a bad token must be refused")
	}
	if len(gate.released) != 0 {
		t.Errorf("nothing must be released on a bad token, released=%v", gate.released)
	}
}

// enqueueOnlyDriver stands in for a queue that only ever inserts rows in the
// decide fixtures; it is never drained.
type enqueueOnlyDriver struct{}

func (enqueueOnlyDriver) Run(context.Context, executor.Spec, executor.EventSink) (executor.Result, error) {
	return executor.Result{}, errors.New("not drained in tests")
}
