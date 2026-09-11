package service_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// TestRunPlaybookCarriesEgressPorts: a playbook run's declared egress ports
// reach the enqueued spec, the hop that makes `playbook run --egress` grant
// the sandboxed run its nonstandard inventory port. Dropping the field from
// the spec assembly in RunPlaybook fails this test.
func TestRunPlaybookCarriesEgressPorts(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())

	if _, err := svc.RunPlaybook(context.Background(), enq, service.PlaybookInput{
		PlaybookPath: "/abs/site.yml",
		EgressPorts:  []int{2222},
	}); err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	if len(enq.enqueued) != 1 {
		t.Fatalf("want one enqueue, got %d", len(enq.enqueued))
	}
	if !slices.Equal(enq.enqueued[0].EgressPorts, []int{2222}) {
		t.Fatalf("spec.EgressPorts = %v, want [2222]", enq.enqueued[0].EgressPorts)
	}
}

// TestRunPlaybookEgressPortsDefaultEmpty: a run that declares nothing keeps an
// empty egress set, so the default-deny posture is preserved at the service
// face.
func TestRunPlaybookEgressPortsDefaultEmpty(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())

	if _, err := svc.RunPlaybook(context.Background(), enq, service.PlaybookInput{
		PlaybookPath: "/abs/site.yml",
	}); err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	if len(enq.enqueued[0].EgressPorts) != 0 {
		t.Fatalf("a run with no declaration got egress %v", enq.enqueued[0].EgressPorts)
	}
}

// TestBuildPlaybookSpecCarriesRuleEgress: a playbook-action rule that declared
// egress ports carries them onto the spec the reactive builder returns, so a
// rule can reach a nonstandard inventory port exactly as a CLI run can.
// Dropping rule.Action.Egress from BuildPlaybookSpec fails this test.
func TestBuildPlaybookSpecCarriesRuleEgress(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "plays"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "plays", "site.yml"), []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	if _, err := st.CreateSource(ctx, store.Source{Name: "ops", Kind: store.SourcePath, URI: checkout, Enabled: true}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	svc := service.New(st, t.TempDir(), nil)

	spec, err := svc.BuildPlaybookSpec(ctx, reactive.Event{}, store.Rule{
		Name: "rule-test", ActionKind: store.ActionPlaybook,
		Action: store.RuleAction{
			Source: "ops", Playbook: "plays/site.yml", Limit: "laptop", Egress: []int{2222},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlaybookSpec: %v", err)
	}
	if !slices.Equal(spec.EgressPorts, []int{2222}) {
		t.Fatalf("spec.EgressPorts = %v, want [2222]", spec.EgressPorts)
	}
}

// TestBuildPlaybookSpecEgressDefaultEmpty: a rule that declared no egress
// builds a spec with none, so the default-deny posture holds for rules.
func TestBuildPlaybookSpecEgressDefaultEmpty(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	checkout := t.TempDir()
	if _, err := st.CreateSource(ctx, store.Source{Name: "ops", Kind: store.SourcePath, URI: checkout, Enabled: true}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "site.yml"), []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	svc := service.New(st, t.TempDir(), nil)

	spec, err := svc.BuildPlaybookSpec(ctx, reactive.Event{}, store.Rule{
		Name: "rule-test", ActionKind: store.ActionPlaybook,
		Action: store.RuleAction{Source: "ops", Playbook: "site.yml", Limit: "laptop"},
	})
	if err != nil {
		t.Fatalf("BuildPlaybookSpec: %v", err)
	}
	if len(spec.EgressPorts) != 0 {
		t.Fatalf("a rule with no declaration got egress %v", spec.EgressPorts)
	}
}
