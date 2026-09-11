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

// TestRunPlaybookCarriesSSHIdentityFiles: a playbook run's declared ssh
// identity files reach the enqueued spec, the hop that makes
// `playbook run --ssh-identity` grant the sandboxed run its external key.
// Dropping the field from the spec assembly in RunPlaybook fails this test.
func TestRunPlaybookCarriesSSHIdentityFiles(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())

	if _, err := svc.RunPlaybook(context.Background(), enq, service.PlaybookInput{
		PlaybookPath:     "/abs/site.yml",
		SSHIdentityFiles: []string{"/keys/lima"},
	}); err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	if len(enq.enqueued) != 1 {
		t.Fatalf("want one enqueue, got %d", len(enq.enqueued))
	}
	if !slices.Equal(enq.enqueued[0].SSHIdentityFiles, []string{"/keys/lima"}) {
		t.Fatalf("spec.SSHIdentityFiles = %v, want [/keys/lima]", enq.enqueued[0].SSHIdentityFiles)
	}
}

// TestRunPlaybookSSHIdentityFilesDefaultEmpty: a run that declares nothing
// keeps an empty identity set, so the default-deny posture is preserved at the
// service face.
func TestRunPlaybookSSHIdentityFilesDefaultEmpty(t *testing.T) {
	enq := &fakeEnqueuer{}
	svc := service.New(nil, t.TempDir(), fixedClock())

	if _, err := svc.RunPlaybook(context.Background(), enq, service.PlaybookInput{
		PlaybookPath: "/abs/site.yml",
	}); err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	if len(enq.enqueued[0].SSHIdentityFiles) != 0 {
		t.Fatalf("a run with no declaration got identity files %v", enq.enqueued[0].SSHIdentityFiles)
	}
}

// TestBuildPlaybookSpecCarriesRuleSSHIdentity: a playbook-action rule that
// declared ssh identity files carries them onto the spec the reactive builder
// returns, so a rule can load an external key exactly as a CLI run can.
// Dropping rule.Action.SSHIdentityFiles from BuildPlaybookSpec fails this
// test.
func TestBuildPlaybookSpecCarriesRuleSSHIdentity(t *testing.T) {
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
			Source: "ops", Playbook: "plays/site.yml", Limit: "laptop",
			SSHIdentityFiles: []string{"/keys/lima"},
		},
	})
	if err != nil {
		t.Fatalf("BuildPlaybookSpec: %v", err)
	}
	if !slices.Equal(spec.SSHIdentityFiles, []string{"/keys/lima"}) {
		t.Fatalf("spec.SSHIdentityFiles = %v, want [/keys/lima]", spec.SSHIdentityFiles)
	}
}

// TestBuildPlaybookSpecSSHIdentityDefaultEmpty: a rule that declared no
// identity files builds a spec with none, so the default-deny posture holds
// for rules.
func TestBuildPlaybookSpecSSHIdentityDefaultEmpty(t *testing.T) {
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
	if len(spec.SSHIdentityFiles) != 0 {
		t.Fatalf("a rule with no declaration got identity files %v", spec.SSHIdentityFiles)
	}
}
