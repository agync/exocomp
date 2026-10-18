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

// TestRearmStepCarriesCaptures proves crash-resume carries captures forward: a
// bootstrap play step re-armed after a mid-run crash must keep its capture
// wiring, or the resumed play seals nothing and the dependent apply's
// ab:<workflow> binding stays empty. It drives rearmStep directly against a step
// whose job carries Captures (plus ExtraVars and TolerateUnreachable, the two
// other replay fields the old rearmStep dropped), and asserts the fresh gated job
// carries them identically.
//
// Mutation check: dropping `Captures: job.Args.Captures` from rearmStep's Spec
// makes newJob.Args.Captures nil, tripping the Captures assertion below — the
// exact loss a crashed-then-resumed bootstrap play suffered.
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
	// threaded it through EnqueueGated onto the job row itself, which is what a
	// re-arm then has to preserve.
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

// assertReplayFields checks the durable grant and identity fields a re-armed
// step must keep from the job it was admitted with. It reports every mismatch
// rather than stopping at the first, so one run names all the fields a broken
// re-arm dropped.
func assertReplayFields(t *testing.T, label string, job store.Job) {
	t.Helper()
	if job.Args.Source != "fleet" {
		t.Errorf("%s: Source = %q, want fleet", label, job.Args.Source)
	}
	if !job.Args.DirReadOnly {
		t.Errorf("%s: DirReadOnly = false, want true", label)
	}
	if !job.Args.Privileged {
		t.Errorf("%s: Privileged = false, want true", label)
	}
	if job.Args.RunnerName != "capped-runner" {
		t.Errorf("%s: RunnerName = %q, want capped-runner", label, job.Args.RunnerName)
	}
	if got, want := job.Args.EgressPorts, []int{443, 8443}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s: EgressPorts = %v, want %v", label, got, want)
	}
	if got, want := job.Args.SSHIdentityFiles, []string{"/etc/exocomp/keys/deploy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s: SSHIdentityFiles = %v, want %v", label, got, want)
	}
	if job.Args.Dir != "/srv/sources/fleet" {
		t.Errorf("%s: Dir = %q, want /srv/sources/fleet", label, job.Args.Dir)
	}
	if job.ConcurrencyKey != "fleet:deploy" {
		t.Errorf("%s: ConcurrencyKey = %q, want fleet:deploy", label, job.ConcurrencyKey)
	}
	if job.DedupKey != "alertmanager:fp:disk" {
		t.Errorf("%s: DedupKey = %q, want alertmanager:fp:disk", label, job.DedupKey)
	}
}

// TestRearmStepCarriesGrantFields proves a crash-resumed workflow step keeps
// every durable replay field its job was admitted with, not a hand-picked
// subset. A re-armed step whose literal is missing Source, DirReadOnly,
// Privileged, RunnerName, EgressPorts, or SSHIdentityFiles runs with a
// different grant: a read-only checkout turned writable, a source-scoped
// egress set dropped, a privileged step silently downgraded. DedupKey is
// carried too because an alert that resolves finds a pending approval by the
// job's dedup_key, so a re-armed step must keep it (Decision 0023 prerequisite).
//
// Mutation check: dropping any carried field from rearmStep's Spec, or the
// DirReadOnly/Privileged pair from EnqueueGated, trips the matching assertion
// on the gated job (the Spec->JobArgs threading guard) or on the re-armed job.
func TestRearmStepCarriesGrantFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q, st := newSeqQueue(t, filepath.Join(dir, "exocomp.db"))
	seq := NewSequencer(q)
	ctx := context.Background()

	const wf = "ansible:wf:rearm-grants"
	play := Step{
		Kind: "ansible-play",
		Spec: Spec{
			Runbook:          "playbook",
			Kind:             "ansible",
			Trigger:          "workflow",
			Argv:             []string{"/usr/bin/ansible-playbook", "site.yml"},
			Dir:              "/srv/sources/fleet",
			DirReadOnly:      true,
			Source:           "fleet",
			RunnerName:       "capped-runner",
			Privileged:       true,
			EgressPorts:      []int{443, 8443},
			SSHIdentityFiles: []string{"/etc/exocomp/keys/deploy"},
			ConcurrencyKey:   "fleet:deploy",
			DedupKey:         "alertmanager:fp:disk",
		},
	}
	if err := seq.Start(ctx, wf, []Step{play}); err != nil {
		t.Fatalf("start: %v", err)
	}

	step := mustSteps(t, st, wf)[0]
	if step.JobID == nil {
		t.Fatal("gated step has no job id")
	}
	orig, err := st.GetJob(ctx, *step.JobID)
	if err != nil {
		t.Fatalf("get gated job: %v", err)
	}
	// The gated job the ledger references must already hold the grant: if the
	// Spec->JobArgs threading (EnqueueGated) dropped it, the resume has nothing
	// to carry and a read-only source silently becomes writable.
	assertReplayFields(t, "gated job", orig)

	// Crash mid-run: the ledger step is running over an interrupted job, the
	// exact shape runStep hands rearmStep.
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
	if rearmed.ID == orig.ID {
		t.Fatal("rearm reused the crashed job rather than enqueueing a fresh one")
	}
	if rearmed.State != store.StateAwaitingApproval {
		t.Errorf("re-armed job is %s, want gated at awaiting_approval", rearmed.State)
	}
	assertReplayFields(t, "re-armed job", rearmed)
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
