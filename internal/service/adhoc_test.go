package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// TestRunAdhocAssemblesAnsibleSpec proves a plain (non-gated) ad-hoc run
// enqueues once, plainly, with the module/args/inventory/passthrough all
// carried onto the spec.
func TestRunAdhocAssemblesAnsibleSpec(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())

	res, err := svc.RunAdhoc(context.Background(), enq, fixedDeriver{}, service.AdhocInput{
		Pattern:        "web",
		Module:         "shell",
		Args:           "systemctl status nginx",
		Become:         true,
		Dir:            "/srv/checkout",
		InventoryPath:  "/abs/hosts.ini",
		InventoryName:  "prod",
		VaultProject:   "vault-a",
		RunSecrets:     "aws",
		Source:         "my-fleet",
		IdempotencyKey: "idem-1",
		Timeout:        30 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunAdhoc: %v", err)
	}
	if len(enq.enqueued) != 1 || len(enq.gated) != 0 {
		t.Fatalf("want one plain enqueue and no gate, got %d/%d", len(enq.enqueued), len(enq.gated))
	}
	spec := enq.enqueued[0]
	if spec.Runbook != "command" || spec.Kind != executor.KindAnsible || spec.Trigger != "manual" {
		t.Errorf("spec header = %q/%q/%q", spec.Runbook, spec.Kind, spec.Trigger)
	}
	argv := strings.Join(spec.Argv, " ")
	if !strings.Contains(argv, "web") || !strings.Contains(argv, "/abs/hosts.ini") ||
		!strings.Contains(argv, "shell") || !strings.Contains(argv, "systemctl status nginx") ||
		!strings.Contains(argv, "--become") {
		t.Errorf("argv did not carry pattern/inventory/module/args/become: %v", spec.Argv)
	}
	if spec.Dir != "/srv/checkout" || spec.VaultProject != "vault-a" ||
		spec.RunSecretBinding != "aws" || spec.Inventory != "prod" {
		t.Errorf("bindings lost: %+v", spec)
	}
	if spec.Source != "my-fleet" {
		t.Errorf("spec.Source = %q, want %q", spec.Source, "my-fleet")
	}
	if spec.IdempotencyKey != "idem-1" || spec.Timeout != 30*time.Second {
		t.Errorf("run options lost: %+v", spec)
	}
	if res.Gated || res.Run == nil || res.Run.Job.ID != 1 {
		t.Fatalf("want a plain run with job id 1, got %+v", res)
	}
}

// TestRunAdhocDefaultsModuleAndRunbook proves an empty Module/Runbook default
// to "command", the everyday ad-hoc shape.
func TestRunAdhocDefaultsModuleAndRunbook(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())

	if _, err := svc.RunAdhoc(context.Background(), enq, fixedDeriver{}, service.AdhocInput{
		Pattern: "web",
	}); err != nil {
		t.Fatalf("RunAdhoc: %v", err)
	}
	spec := enq.enqueued[0]
	if spec.Runbook != "command" {
		t.Errorf("runbook = %q, want command", spec.Runbook)
	}
	if !strings.Contains(strings.Join(spec.Argv, " "), "command") {
		t.Errorf("argv missing default module: %v", spec.Argv)
	}
}

// TestRunAdhocGuardsHostPattern proves a dash-led or control-char pattern is
// refused before anything is enqueued.
func TestRunAdhocGuardsHostPattern(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())

	for _, pattern := range []string{"", "-x", "web\x00"} {
		if _, err := svc.RunAdhoc(context.Background(), enq, fixedDeriver{}, service.AdhocInput{
			Pattern: pattern,
		}); err == nil {
			t.Errorf("pattern %q: want a guard error", pattern)
		}
	}
	if len(enq.enqueued)+len(enq.gated) != 0 {
		t.Error("a refused pattern must not enqueue anything")
	}
}

// TestRunAdhocGuardsModuleArgs proves a dash-led -a string is refused.
func TestRunAdhocGuardsModuleArgs(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())

	if _, err := svc.RunAdhoc(context.Background(), enq, fixedDeriver{}, service.AdhocInput{
		Pattern: "web", Args: "-rf /",
	}); err == nil {
		t.Fatal("want a guard error for module args starting with a dash")
	}
	if len(enq.enqueued) != 0 {
		t.Error("a refused args string must not enqueue anything")
	}
}

// TestRunAdhocPrivilegedMarksSpecAndLedger proves the operator-gated privileged
// request reaches the spec and is recorded, attributed, on the job ledger — the
// durable evidence a run was authorised to skip the sandbox.
func TestRunAdhocPrivilegedMarksSpecAndLedger(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	enq := &recordingEnqueuer{q: q}
	svc := service.New(st, t.TempDir(), fixedClock())

	res, err := svc.RunAdhoc(ctx, enq, fixedDeriver{}, service.AdhocInput{
		Pattern:    "web",
		Args:       "uptime",
		Privileged: true,
		Actor:      service.Actor{Kind: service.ActorKindToken, Name: "ops-token", Face: "api"},
	})
	if err != nil {
		t.Fatalf("RunAdhoc: %v", err)
	}
	if len(enq.enqueued) != 1 || !enq.enqueued[0].Privileged {
		t.Fatalf("privileged did not reach the spec: %+v", enq.enqueued)
	}
	events, err := st.ListJobEvents(ctx, res.Run.Job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if strings.Contains(e.Msg, "UNSANDBOXED (privileged)") && strings.Contains(e.Msg, "ops-token") {
			found = true
		}
	}
	if !found {
		t.Errorf("no attributed privileged event on the ledger: %+v", events)
	}
}

func TestRunAdhocPropagatesEnqueueError(t *testing.T) {
	enq := &fakeEnqueuer{err: errors.New("boom")}
	svc := service.New(nil, t.TempDir(), fixedClock())
	if _, err := svc.RunAdhoc(context.Background(), enq, fixedDeriver{}, service.AdhocInput{
		Pattern: "web",
	}); err == nil {
		t.Fatal("want the enqueue error surfaced")
	}
}

// TestRunAdhocRequireApprovalGates is the guardrail: RequireApproval must
// EnqueueGated (never Enqueue) and create a pending approval.
func TestRunAdhocRequireApprovalGates(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	q := queue.New(st, enqueueOnlyDriver{}, queue.Options{Workers: 1})
	enq := &recordingEnqueuer{q: q}
	svc := service.New(st, t.TempDir(), fixedClock())

	res, err := svc.RunAdhoc(ctx, enq, fixedDeriver{}, service.AdhocInput{
		Pattern:         "web",
		Module:          "command",
		Args:            "uptime",
		RequireApproval: true,
	})
	if err != nil {
		t.Fatalf("RunAdhoc: %v", err)
	}
	if len(enq.enqueued) != 0 || len(enq.gated) != 1 {
		t.Fatalf("a require_approval run must EnqueueGated, not Enqueue: %d plain / %d gated",
			len(enq.enqueued), len(enq.gated))
	}
	if !res.Gated || res.Run != nil {
		t.Fatalf("want a gated result, got %+v", res)
	}
	gatedJob, err := st.GetJob(ctx, res.Approval.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if gatedJob.State != store.StateAwaitingApproval {
		t.Errorf("gated job state = %q, want awaiting_approval", gatedJob.State)
	}
	stored, err := st.GetApproval(ctx, res.Approval.ApprovalID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if stored.JobID != res.Approval.JobID {
		t.Errorf("stored approval job = %d, want %d", stored.JobID, res.Approval.JobID)
	}
	if stored.TokenHash == "" {
		t.Error("the approval must store a token hash; the token itself is never persisted")
	}
}
