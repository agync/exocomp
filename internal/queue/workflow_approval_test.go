package queue_test

// End-to-end proof that a composed chain's --require-approval step is durably
// gated: the daemon runs, the sequencer drives, and the gated step's job does
// NOT run until the SAME DecideApproval path a single gated apply uses settles
// it. These live in an external test package so they can drive the real reactive
// engine (which imports queue) against a real queue daemon.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// approvalDeriver is a deterministic stand-in for the secrets envelope, enough
// for the capability an approval row stores and Decide re-derives.
type approvalDeriver struct{}

func (approvalDeriver) Derive(purpose, id string) []byte {
	return []byte("approval-test-" + purpose + "-" + id)
}

// gateHarness wires a real store, a real queue draining a shell driver, and a
// real reactive engine — reactive.New registers the engine as the queue's step
// approver, exactly as the daemon's construction does.
type gateHarness struct {
	dir     string
	store   *store.Store
	queue   *queue.Queue
	seq     *queue.Sequencer
	engine  *reactive.Engine
	markers string
}

func newGateHarness(t *testing.T) *gateHarness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      2,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	eng, err := reactive.New(reactive.Options{
		Store:   st,
		Queue:   q,
		Deriver: approvalDeriver{},
		Clock:   func() time.Time { return time.Now().UTC() },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("reactive engine: %v", err)
	}
	return &gateHarness{
		dir:     dir,
		store:   st,
		queue:   q,
		seq:     queue.NewSequencer(q),
		engine:  eng,
		markers: filepath.Join(dir, "markers"),
	}
}

// threeStepChain appends a label per step; the file's contents prove ordering.
// Step 2 is the gated middle step.
func (h *gateHarness) threeStepChain() []queue.Step {
	mk := func(label string) queue.Step {
		return queue.Step{Kind: "shell", Spec: queue.Spec{
			Runbook: "wf", Kind: "shell", Trigger: "workflow",
			Argv: []string{"/bin/sh", "-c", "echo " + label + " >> " + h.markers},
		}}
	}
	s1 := mk("one")
	s2 := mk("two")
	s2.RequireApproval = true
	s3 := mk("three")
	return []queue.Step{s1, s2, s3}
}

// drive starts the daemon and a sequencer loop mirroring the daemon's
// driveWorkflows: every tick it re-drives incomplete workflows. Both stop when
// the returned cancel is called.
func (h *gateHarness) drive(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	daemonDone := make(chan struct{})
	go func() { defer close(daemonDone); _ = h.queue.Run(ctx) }()

	seqDone := make(chan struct{})
	go func() {
		defer close(seqDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			_, _ = h.seq.ResumeAll(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-daemonDone:
		case <-time.After(30 * time.Second):
			t.Error("daemon did not stop")
		}
		<-seqDone
	})
	return cancel
}

func readMarkerFields(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	return strings.Fields(string(data))
}

func stepStatus(t *testing.T, st *store.Store, id string, stepNo int) store.WorkflowStep {
	t.Helper()
	s, err := st.GetWorkflowStep(context.Background(), id, stepNo)
	if err != nil {
		t.Fatalf("get step %d: %v", stepNo, err)
	}
	return s
}

// waitGateHolds blocks until the chain has run step 1 and armed the gated step's
// approval, and fails fast if the gated step ran without an approval — the exact
// defeat, and the signal a reverted guard trips.
func waitGateHolds(t *testing.T, h *gateHarness, id string) store.Approval {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		// The gated step's effect must never appear before an approval.
		for _, m := range readMarkerFields(t, h.markers) {
			if m == "two" {
				t.Fatal("gated step ran before any approval: the daemon defeated the gate")
			}
		}
		pending, err := h.store.ListApprovals(context.Background(), true)
		if err != nil {
			t.Fatalf("list approvals: %v", err)
		}
		if len(pending) == 1 && stepStatus(t, h.store, id, 1).Status == store.StepSucceeded {
			return pending[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gate never armed: step 1 did not succeed or no approval was created")
	return store.Approval{}
}

func waitStepStatus(t *testing.T, st *store.Store, id string, stepNo int, want store.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s, err := st.GetWorkflowStep(context.Background(), id, stepNo)
		if err != nil {
			t.Fatalf("get step %d: %v", stepNo, err)
		}
		if s.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workflow %s step %d never reached %s", id, stepNo, want)
}

func waitWorkflow(t *testing.T, st *store.Store, id string, want store.WorkflowState) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := st.GetWorkflow(context.Background(), id)
		if err != nil {
			t.Fatalf("get workflow: %v", err)
		}
		if store.WorkflowStatus(steps) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workflow %s never reached %s", id, want)
}

// decide settles the pending approval through the real engine — the same
// DecideApproval capability path a single gated apply uses. The token is
// re-derived from the envelope, exactly as `exocomp approval` does.
func (h *gateHarness) decide(t *testing.T, a store.Approval, decision string) {
	t.Helper()
	token, _, err := reactive.ApprovalToken(approvalDeriver{}, a.JobID)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if _, err := h.engine.Decide(context.Background(), a.ID, decision, token, "test"); err != nil {
		t.Fatalf("decide %s: %v", decision, err)
	}
}

// TestComposedChainGatedStepHeldUntilApproved is the primary gate: a composed
// chain with a gated middle step does not run that step (or anything after)
// while a daemon runs, until the approval is settled; approving releases exactly
// that step and the chain continues.
//
// Mutation check: remove the require-approval branch in Sequencer.runStep (fall
// through to the unconditional Release). waitGateHolds then either sees "two"
// before any approval (fast kill) or never sees an armed approval (timeout kill).
func TestComposedChainGatedStepHeldUntilApproved(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t)
	ctx := context.Background()

	if err := h.seq.Start(ctx, "wf-gate", h.threeStepChain()); err != nil {
		t.Fatalf("start: %v", err)
	}
	h.drive(t)

	// The gate holds: step 1 ran, step 2 is armed and awaiting, nothing after ran.
	approval := waitGateHolds(t, h, "wf-gate")
	if got := readMarkerFields(t, h.markers); len(got) != 1 || got[0] != "one" {
		t.Fatalf("markers = %v, want [one] while the gate holds", got)
	}
	step2 := stepStatus(t, h.store, "wf-gate", 2)
	if step2.JobID == nil {
		t.Fatal("gated step has no job")
	}
	job, err := h.store.GetJob(ctx, *step2.JobID)
	if err != nil {
		t.Fatalf("get gated job: %v", err)
	}
	if job.State != store.StateAwaitingApproval {
		t.Fatalf("gated job is %s, want awaiting_approval while unapproved", job.State)
	}
	// The claim path itself refuses the unsettled job: even forced to queued, a
	// running daemon must not run it.
	if err := h.store.ClaimJob(ctx, job.ID); !errors.Is(err, store.ErrStateConflict) {
		t.Fatalf("claim of an awaiting job = %v, want a state conflict (unclaimable)", err)
	}

	// Approve through the real capability path; exactly that step proceeds.
	h.decide(t, approval, reactive.DecisionApprove)
	waitWorkflow(t, h.store, "wf-gate", store.WorkflowSucceeded)

	got := readMarkerFields(t, h.markers)
	if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("markers = %v, want [one two three] after approval", got)
	}
}

// TestComposedChainRejectBlocksDownstream proves rejecting the gated step stops
// the chain: the gated step fails and every step after it is blocked, and
// neither the gated step nor anything downstream ever ran.
func TestComposedChainRejectBlocksDownstream(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t)
	ctx := context.Background()

	if err := h.seq.Start(ctx, "wf-reject", h.threeStepChain()); err != nil {
		t.Fatalf("start: %v", err)
	}
	h.drive(t)

	approval := waitGateHolds(t, h, "wf-reject")
	h.decide(t, approval, reactive.DecisionReject)
	// WorkflowStatus reports failed the instant the gated step fails, one write
	// before the downstream step is blocked, so wait on the terminal downstream
	// status itself rather than the derived workflow state.
	waitStepStatus(t, h.store, "wf-reject", 3, store.StepBlocked)

	steps, err := h.store.GetWorkflow(ctx, "wf-reject")
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if steps[0].Status != store.StepSucceeded {
		t.Errorf("step 1 = %s, want succeeded", steps[0].Status)
	}
	if steps[1].Status != store.StepFailed {
		t.Errorf("gated step = %s, want failed after reject", steps[1].Status)
	}
	if store.WorkflowStatus(steps) != store.WorkflowFailed {
		t.Errorf("workflow = %s, want failed", store.WorkflowStatus(steps))
	}
	if got := readMarkerFields(t, h.markers); len(got) != 1 || got[0] != "one" {
		t.Fatalf("markers = %v, want [one]: neither the rejected step nor anything after ran", got)
	}
}
