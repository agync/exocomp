package reactive_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// fakePlaybookBuilder stands in for the daemon's service-backed builder: it
// shapes only the executable half of a spec and never the identity fields,
// which the engine must stamp.
type fakePlaybookBuilder struct{ err error }

func (f fakePlaybookBuilder) BuildPlaybookSpec(_ context.Context, _ reactive.Event, rule store.Rule) (queue.Spec, error) {
	if f.err != nil {
		return queue.Spec{}, f.err
	}
	return queue.Spec{
		Kind:   executor.KindAnsible,
		Argv:   []string{executor.AnsibleBinary, rule.Action.Playbook},
		Source: rule.Action.Source,
	}, nil
}

// playbookHarness opens a store and a real queue (so a gated job row and its
// approval FK exist) and wires a PlaybookBuilder into the engine — or leaves
// it nil, the daemonless construction.
func playbookHarness(t *testing.T, b reactive.PlaybookBuilder) (*store.Store, *reactive.Engine) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      2,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.DiscardHandler),
	})
	engine, err := reactive.New(reactive.Options{
		Store:     st,
		Queue:     q,
		Deriver:   testDeriver{seed: "playbook-test"},
		Playbooks: b,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return st, engine
}

func playbookRule(t *testing.T, st *store.Store, requireApproval bool) {
	t.Helper()
	if _, err := st.CreateRule(context.Background(), store.NewRule{
		Name:            "laptop-update",
		SourceFilter:    "generic",
		CELExpr:         `event.name == "home" && event.status == "firing"`,
		ActionKind:      store.ActionPlaybook,
		Action:          store.RuleAction{Source: "ops", Playbook: "plays/site.yml", Limit: "laptop"},
		RequireApproval: requireApproval,
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}
}

// TestPlaybookActionStampsIdentity: a matched playbook rule reaches the queue
// with the identity fields the engine owns — Runbook "rule:<name>", the
// event's DedupKey, ConcurrencyKey "rule:<name>" — stamped on top of the
// builder's executable half.
func TestPlaybookActionStampsIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, engine := playbookHarness(t, fakePlaybookBuilder{})
	playbookRule(t, st, false)

	ev := reactive.Event{
		Source: "generic", Kind: "presence", Name: "home",
		Status: "firing", DedupKey: "generic:home",
	}
	stored, err := engine.Ingest(ctx, ev, []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stored.Outcome != store.OutcomeFired || stored.JobID == nil {
		t.Fatalf("event = %+v, want a fired event naming a job", stored)
	}
	job, err := st.GetJob(ctx, *stored.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Runbook != "rule:laptop-update" {
		t.Errorf("runbook = %q, want rule:laptop-update", job.Runbook)
	}
	if job.Trigger != reactive.Trigger {
		t.Errorf("trigger = %q, want event", job.Trigger)
	}
	if job.DedupKey != ev.DedupKey {
		t.Errorf("dedup key = %q, want %q", job.DedupKey, ev.DedupKey)
	}
	if job.ConcurrencyKey != "rule:laptop-update" {
		t.Errorf("concurrency key = %q, want rule:laptop-update", job.ConcurrencyKey)
	}
	if job.Kind != executor.KindAnsible || job.Args.Source != "ops" {
		t.Errorf("job = %+v, want the builder's executable half preserved", job)
	}
}

// TestPlaybookActionWithoutBuilderFailsClosed: an engine wired with no
// PlaybookBuilder (the web engine and the CLI approval engine) records an
// outcome error for a matching playbook rule and fires nothing. Mutation
// check: removing the nil guard turns this into a nil-interface panic and the
// test fails.
func TestPlaybookActionWithoutBuilderFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, engine := playbookHarness(t, nil)
	playbookRule(t, st, false)

	stored, err := engine.Ingest(ctx, reactive.Event{
		Source: "generic", Kind: "presence", Name: "home",
		Status: "firing", DedupKey: "generic:home",
	}, []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stored.Outcome != store.OutcomeError {
		t.Fatalf("outcome = %s, want error", stored.Outcome)
	}
	if !strings.Contains(stored.Reason, "no builder wired") {
		t.Errorf("reason = %q, want the nil-builder guard message", stored.Reason)
	}
	if stored.JobID != nil {
		t.Error("a builder-less playbook rule must not enqueue a job")
	}
	jobs, err := st.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d job(s) reached the queue without a builder", len(jobs))
	}
}

// TestPlaybookActionRequireApprovalGates: a require-approval playbook rule
// routes through gateAction into an awaiting_approval job, the same as any
// other gated action.
func TestPlaybookActionRequireApprovalGates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, engine := playbookHarness(t, fakePlaybookBuilder{})
	playbookRule(t, st, true)

	stored, err := engine.Ingest(ctx, reactive.Event{
		Source: "generic", Kind: "presence", Name: "home",
		Status: "firing", DedupKey: "generic:home",
	}, []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stored.Outcome != store.OutcomeGated || stored.JobID == nil {
		t.Fatalf("event = %+v, want a gated event naming a job", stored)
	}
	job, err := st.GetJob(ctx, *stored.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != store.StateAwaitingApproval {
		t.Errorf("job state = %q, want awaiting_approval", job.State)
	}
	if job.Runbook != "rule:laptop-update" || job.ConcurrencyKey != "rule:laptop-update" {
		t.Errorf("gated job identity = %q/%q, want the engine-stamped values",
			job.Runbook, job.ConcurrencyKey)
	}
}
