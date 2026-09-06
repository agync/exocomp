package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// actorPayload mirrors service's unexported actorEventPayload for decoding a
// recorded event's RawJSON in tests.
type actorPayload struct {
	ActorKind string `json:"actor_kind"`
	ActorName string `json:"actor_name"`
	Face      string `json:"face"`
}

// TestPlanWorkspaceEmptyActorRecordsNoEvent proves the zero Actor — what every
// existing CLI caller passes — leaves the job's event ledger exactly as it was
// before this change: empty until a daemon claims the row.
func TestPlanWorkspaceEmptyActorRecordsNoEvent(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	enq := &recordingEnqueuer{q: q}
	svc := service.New(st, t.TempDir(), fixedClock())

	res, err := svc.PlanWorkspace(ctx, enq, service.PlanInput{
		Workspace: "web", TofuDir: t.TempDir(), ArtifactsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("PlanWorkspace: %v", err)
	}
	events, err := st.ListJobEvents(ctx, res.Job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("empty Actor recorded %d event(s), want 0 (today's behavior unchanged): %+v", len(events), events)
	}
}

// TestPlanWorkspaceRecordsActor proves a present Actor lands a system event on
// the job's own append-only ledger — token name only, structured, face "api".
func TestPlanWorkspaceRecordsActor(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	enq := &recordingEnqueuer{q: q}
	svc := service.New(st, t.TempDir(), fixedClock())

	res, err := svc.PlanWorkspace(ctx, enq, service.PlanInput{
		Workspace: "web", TofuDir: t.TempDir(), ArtifactsDir: t.TempDir(),
		Actor: service.Actor{Kind: service.ActorKindToken, Name: "ci-bot"},
	})
	if err != nil {
		t.Fatalf("PlanWorkspace: %v", err)
	}
	events, err := st.ListJobEvents(ctx, res.Job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d event(s), want exactly 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Level != store.LevelSystem {
		t.Errorf("level = %q, want %q", ev.Level, store.LevelSystem)
	}
	var payload actorPayload
	if err := json.Unmarshal([]byte(ev.RawJSON), &payload); err != nil {
		t.Fatalf("actor event RawJSON did not decode: %v (%s)", err, ev.RawJSON)
	}
	if payload.ActorKind != service.ActorKindToken || payload.ActorName != "ci-bot" || payload.Face != "api" {
		t.Errorf("payload = %+v, want kind=token name=ci-bot face=api", payload)
	}
}

// TestPlanWorkspacePropagatesActorRecordError proves the append failing fails
// the write too, instead of being swallowed like ordinary run telemetry: the
// closed store fails only the actor append, isolating the assertion from the
// unrelated CreateJob path (the fake enqueuer never touches the store).
func TestPlanWorkspacePropagatesActorRecordError(t *testing.T) {
	st := openStore(t)
	_ = st.Close()
	enq := &fakeEnqueuer{}
	svc := service.New(st, t.TempDir(), fixedClock())

	_, err := svc.PlanWorkspace(context.Background(), enq, service.PlanInput{
		Workspace: "web", TofuDir: "/roots/web", ArtifactsDir: t.TempDir(),
		Actor: service.Actor{Kind: service.ActorKindToken, Name: "ci"},
	})
	if err == nil {
		t.Fatal("want the actor-record error surfaced, not swallowed")
	}
}

// TestApplyWorkspacePlainRecordsActor covers the plain (non-gated) apply path.
func TestApplyWorkspacePlainRecordsActor(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	enq := &recordingEnqueuer{q: q}
	svc := service.New(st, t.TempDir(), fixedClock())

	res, err := svc.ApplyWorkspace(ctx, enq, fixedDeriver{}, service.ApplyInput{
		Workspace: "web", TofuDir: "/roots/web",
		Actor: service.Actor{Kind: service.ActorKindToken, Name: "op"},
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		return executor.TofuArgv("apply", "-auto-approve"), nil, func() {}, nil
	})
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	events, err := st.ListJobEvents(ctx, res.Run.Job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d event(s), want 1: %+v", len(events), events)
	}
	if events[0].RawJSON == "" {
		t.Fatal("actor event carries no payload")
	}
}

// TestApplyWorkspacePlainReleasesOnActorRecordError proves a failed actor
// append still releases the plaintext plan the argv builder opened — the
// write fails, but it must not leak a plaintext plan copy while doing so. It
// drives the saved-plan path, the one whose argv builder opens a plaintext.
func TestApplyWorkspacePlainReleasesOnActorRecordError(t *testing.T) {
	st := openStore(t)
	_ = st.Close()
	enq := &fakeEnqueuer{}
	svc := service.New(st, t.TempDir(), fixedClock())
	released := false

	_, err := svc.ApplyWorkspace(context.Background(), enq, fixedDeriver{}, service.ApplyInput{
		Workspace: "web", TofuDir: "/roots/web",
		SavedPlan: &store.Artifact{ID: 7, Kind: "tofu-plan", Path: "/plans/web.tfplan.age"},
		Actor:     service.Actor{Kind: service.ActorKindToken, Name: "op"},
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		return executor.TofuArgv("apply", "/tmp/plan"), nil, func() { released = true }, nil
	})
	if err == nil {
		t.Fatal("want the actor-record error surfaced")
	}
	if !released {
		t.Error("release must run when actor recording fails, so no plaintext plan is leaked")
	}
}

// TestApplyWorkspaceGatedRecordsActor proves a gated apply's actor is
// recorded on the gated job itself, so the approval it creates (which only
// points at JobID) reaches the same attribution through the job's ledger.
func TestApplyWorkspaceGatedRecordsActor(t *testing.T) {
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

	res, err := svc.ApplyWorkspace(ctx, enq, fixedDeriver{}, service.ApplyInput{
		Workspace: "web", WorkspaceSerial: 4, TofuDir: "/roots/web",
		SavedPlan: &saved,
		Gate:      service.PlanGate{RequireApproval: true, Policy: "prod-guard"},
		Actor:     service.Actor{Kind: service.ActorKindToken, Name: "ci-bot"},
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		return nil, nil, func() {}, nil
	})
	if err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}
	if !res.Gated {
		t.Fatal("want a gated result")
	}

	// The approval only carries JobID — attribution is read back through the
	// gated job's own event ledger, not a new column on the approval row.
	events, err := st.ListJobEvents(ctx, res.Approval.JobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var found bool
	for _, ev := range events {
		var payload actorPayload
		if json.Unmarshal([]byte(ev.RawJSON), &payload) == nil && payload.ActorName == "ci-bot" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gated job's events carry no actor record: %+v", events)
	}
}

// TestRunPlaybookRecordsActor covers the ansible playbook verb.
func TestRunPlaybookRecordsActor(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	enq := &recordingEnqueuer{q: q}
	svc := service.New(st, t.TempDir(), fixedClock())

	res, err := svc.RunPlaybook(ctx, enq, service.PlaybookInput{
		PlaybookPath: "/abs/site.yml", InventoryPath: "/abs/hosts.ini",
		Actor: service.Actor{Kind: service.ActorKindToken, Name: "ci-bot"},
	})
	if err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	events, err := st.ListJobEvents(ctx, res.Job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d event(s), want 1: %+v", len(events), events)
	}
}

// TestJobActorReturnsFace proves JobActor reads the recorded surface back out of
// the ledger: a web-enqueued job's actor event carries Face "web", and the read
// returns it so the job detail sentence can render "from the web". The zero
// Actor (no event) reads back empty.
func TestJobActorReturnsFace(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	enq := &recordingEnqueuer{q: q}
	svc := service.New(st, t.TempDir(), fixedClock())

	res, err := svc.RunPlaybook(ctx, enq, service.PlaybookInput{
		PlaybookPath: "/abs/site.yml", InventoryPath: "/abs/hosts.ini",
		Actor: service.Actor{Kind: service.ActorKindToken, Name: "laptop", Face: "web"},
	})
	if err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	actor, err := svc.JobActor(ctx, res.Job.ID)
	if err != nil {
		t.Fatalf("JobActor: %v", err)
	}
	if actor.Kind != service.ActorKindToken || actor.Name != "laptop" || actor.Face != "web" {
		t.Errorf("actor = %+v, want kind=token name=laptop face=web", actor)
	}

	// An API write records the default face.
	other, err := svc.RunPlaybook(ctx, enq, service.PlaybookInput{
		PlaybookPath: "/abs/other.yml", InventoryPath: "/abs/hosts.ini",
		Actor: service.Actor{Kind: service.ActorKindToken, Name: "ci"},
	})
	if err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	actor, err = svc.JobActor(ctx, other.Job.ID)
	if err != nil {
		t.Fatalf("JobActor: %v", err)
	}
	if actor.Face != "api" {
		t.Errorf("default face = %q, want api", actor.Face)
	}
}
