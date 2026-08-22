package queue

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestNonDecision pins the resume distinction the sequencer turns on: a step
// whose job did not reach a decision is re-run, one that did is settled. The
// subtle case is cancelled — a shutdown that killed a RUNNING step re-runs,
// while an approval the gate denied (cancelled before it ever started) does
// not, so a denied step blocks the workflow instead of looping forever.
func TestNonDecision(t *testing.T) {
	started := time.Now().UTC()
	cases := []struct {
		name string
		job  store.Job
		want bool
	}{
		{"interrupted crash", store.Job{State: store.StateInterrupted}, true},
		{"shutdown killed a running step",
			store.Job{State: store.StateCancelled, StartedAt: &started}, true},
		{"approval denied before it ran",
			store.Job{State: store.StateCancelled, StartedAt: nil}, false},
		{"failed on its own exit code", store.Job{State: store.StateFailed}, false},
		{"timed out", store.Job{State: store.StateTimedOut}, false},
		{"succeeded", store.Job{State: store.StateSucceeded}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nonDecision(tc.job); got != tc.want {
				t.Errorf("nonDecision(%s, started=%v) = %v, want %v",
					tc.job.State, tc.job.StartedAt != nil, got, tc.want)
			}
		})
	}
}

// TestResumeAllSkipsInlineOnlyWorkflows is the daemon-vs-upgrade gate (backlog
// item 2, evidence for item 1). A crash leaves two incomplete workflows in one
// ledger: a normal shell chain and an operator-inline one (an upgrade
// stand-in). ResumeAll must resume the normal chain and leave the inline one
// EXACTLY as the crash left it — not run, not failed, not re-armed. Leaving it
// pending under its original idempotency key is the item-1 proof that a
// daemon never retargets an interrupted upgrade step (so it cannot double-apply
// a non-idempotent swap): it never touches it at all.
//
// Mutation check: dropping the workflowInlineOnly skip in ResumeAll makes the
// daemon release and run the inline job — inlineMarker appears, its step goes
// succeeded, and resumed becomes 2 — tripping every assertion below.
func TestResumeAllSkipsInlineOnlyWorkflows(t *testing.T) {
	t.Parallel()
	const inlineKind = "test.inline-only"
	RegisterInlineOnlyKind(inlineKind)

	dir := t.TempDir()
	q, st := newSeqQueue(t, filepath.Join(dir, "exocomp.db"))
	seq := NewSequencer(q)
	ctx := context.Background()

	normalMarker := filepath.Join(dir, "normal-ran")
	inlineMarker := filepath.Join(dir, "inline-ran")

	// A normal shell-kind chain the daemon's driver can run: ResumeAll drives it.
	normal := []Step{step("shell", "/usr/bin/touch", normalMarker)}
	if err := seq.Start(ctx, "wf-normal", normal); err != nil {
		t.Fatalf("start normal: %v", err)
	}

	// An operator-inline workflow: its ledger step carries an inline-only kind.
	// The underlying spec is a plain shell touch, so it WOULD run if the daemon
	// released it — which is exactly what the skip must prevent.
	inline := []Step{{
		Kind: inlineKind,
		Spec: Spec{Runbook: "wf", Kind: "shell", Trigger: "workflow",
			Argv: []string{"/usr/bin/touch", inlineMarker}},
	}}
	if err := seq.Start(ctx, "wf-inline", inline); err != nil {
		t.Fatalf("start inline: %v", err)
	}
	inlineKeyBefore := mustSteps(t, st, "wf-inline")[0].IdempotencyKey

	_, cancel, done := runDaemon(t, q)
	defer stopDaemon(t, cancel, done)

	resumed, err := seq.ResumeAll(ctx)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed != 1 {
		t.Fatalf("resumed %d workflows, want 1 (only the normal chain)", resumed)
	}

	// The normal chain ran to completion.
	if _, err := os.Stat(normalMarker); err != nil {
		t.Fatalf("normal workflow did not run: %v", err)
	}
	if got := store.WorkflowStatus(mustSteps(t, st, "wf-normal")); got != store.WorkflowSucceeded {
		t.Fatalf("normal workflow status = %s, want succeeded", got)
	}

	// The inline workflow was left untouched: still pending, same idempotency
	// key (never retargeted/re-armed), its effect never ran, its job never
	// released from the gate.
	inLedger := mustSteps(t, st, "wf-inline")
	if inLedger[0].Status != store.StepPending {
		t.Fatalf("inline step is %s, want left pending (untouched)", inLedger[0].Status)
	}
	if inLedger[0].IdempotencyKey != inlineKeyBefore {
		t.Fatalf("inline step key changed to %q (was %q): ResumeAll re-armed it",
			inLedger[0].IdempotencyKey, inlineKeyBefore)
	}
	if _, err := os.Stat(inlineMarker); err == nil {
		t.Fatal("inline-only workflow ran though ResumeAll must skip it")
	}
	j, err := st.JobByIdempotencyKey(ctx, stepKey("wf-inline", 1))
	if err != nil {
		t.Fatalf("find inline job: %v", err)
	}
	if j.State != store.StateAwaitingApproval {
		t.Errorf("inline job is %s, want left gated at awaiting_approval", j.State)
	}
}
