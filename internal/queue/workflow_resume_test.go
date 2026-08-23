package queue

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

// TestRearmStepCarriesCaptures is the crash-resume gate this lane closes: a
// bootstrap play step re-armed after a mid-run crash must keep its capture
// wiring, or the resumed play seals nothing and the dependent apply's
// ab:<workflow> binding stays empty. It drives rearmStep directly against a step
// whose job carries Captures (plus ExtraVars and TolerateUnreachable, the two
// other replay fields the old rearmStep dropped), and asserts the fresh gated job
// carries them identically.
//
// Mutation check: dropping `Captures: job.Args.Captures` from rearmStep's Spec
// (the gap that existed before this lane) makes newJob.Args.Captures nil, tripping
// the Captures assertion below — the exact loss a crashed-then-resumed bootstrap
// play suffered.
func TestRearmStepCarriesCaptures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q, st := newSeqQueue(t, filepath.Join(dir, "exocomp.db"))
	seq := NewSequencer(q)
	ctx := context.Background()

	const wf = "bootstrap:wsp:resume"
	captures := &store.JobCaptures{
		Workflow: wf,
		Targets: []store.JobCapture{
			{Name: "KUBECONFIG", File: "kubeconfig", AsFile: true},
			{Name: "KUBE_TOKEN", File: "token"},
		},
	}
	extraVars := map[string]string{"env": "prod"}
	play := Step{
		Kind: "ansible-play",
		Spec: Spec{
			Runbook:             "playbook",
			Kind:                "ansible",
			Trigger:             "bootstrap",
			Argv:                []string{"/usr/bin/ansible-playbook", "site.yml"},
			ExtraVars:           extraVars,
			TolerateUnreachable: true,
			Captures:            captures,
		},
	}
	if err := seq.Start(ctx, wf, []Step{play}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The gated job the ledger references already carries the wiring: the Spec
	// threaded it through EnqueueGated onto the row (the store field this lane
	// added), which is what a re-arm then has to preserve.
	step := mustSteps(t, st, wf)[0]
	if step.JobID == nil {
		t.Fatal("play step has no job id")
	}
	orig, err := st.GetJob(ctx, *step.JobID)
	if err != nil {
		t.Fatalf("get gated job: %v", err)
	}
	if orig.Args.Captures == nil {
		t.Fatal("gated job lost its captures before any crash: Spec->JobArgs threading is broken")
	}

	// Simulate a crash mid-run: the ledger step is marked running and its job
	// starts, then a shutdown/boot-sweep retires the job to interrupted — a
	// non-decision the sequencer re-arms rather than records as a failure. This
	// is the exact ledger shape runStep hands rearmStep (a running step over an
	// interrupted job).
	if err := st.StartStep(ctx, wf, step.StepNo, orig.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}
	if err := st.TransitionJob(ctx, orig.ID, store.StateAwaitingApproval, store.StateRunning); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if err := st.TransitionJob(ctx, orig.ID, store.StateRunning, store.StateInterrupted); err != nil {
		t.Fatalf("interrupt job: %v", err)
	}
	crashed, err := st.GetJob(ctx, orig.ID)
	if err != nil {
		t.Fatalf("re-read crashed job: %v", err)
	}
	if !nonDecision(crashed) {
		t.Fatalf("interrupted job is not a non-decision; resume would not re-arm it")
	}

	rearmed, err := seq.rearmStep(ctx, wf, step, crashed)
	if err != nil {
		t.Fatalf("rearm: %v", err)
	}

	// The re-armed job is a fresh gated row under a new key, but carries every
	// durable replay field verbatim — Captures above all.
	if rearmed.ID == orig.ID {
		t.Fatal("rearm reused the crashed job rather than enqueueing a fresh one")
	}
	if rearmed.State != store.StateAwaitingApproval {
		t.Errorf("re-armed job is %s, want gated at awaiting_approval", rearmed.State)
	}
	if !reflect.DeepEqual(rearmed.Args.Captures, captures) {
		t.Errorf("re-armed job captures = %+v, want %+v", rearmed.Args.Captures, captures)
	}
	if !reflect.DeepEqual(rearmed.Args.ExtraVars, extraVars) {
		t.Errorf("re-armed job extra vars = %v, want %v", rearmed.Args.ExtraVars, extraVars)
	}
	if !rearmed.Args.TolerateUnreachable {
		t.Error("re-armed job lost TolerateUnreachable")
	}

	// The ledger step now points at the re-armed job's key, so the resume runs it
	// rather than the crashed original.
	if got := mustSteps(t, st, wf)[0].IdempotencyKey; got == step.IdempotencyKey {
		t.Errorf("ledger step key unchanged (%q): rearm did not retarget it", got)
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
