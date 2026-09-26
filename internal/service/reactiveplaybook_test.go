package service_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// buildPlaybookSpec is the small rule-shaping helper every test case shares:
// it carries a fixed name and a fixed CEL-irrelevant event, so only the action
// fields under test vary.
func buildPlaybookSpec(t *testing.T, svc *service.Service, source, playbook, limit string) (spec queue.Spec, err error) {
	t.Helper()
	return svc.BuildPlaybookSpec(context.Background(), reactive.Event{},
		store.Rule{Name: "rule-test", ActionKind: store.ActionPlaybook, Action: store.RuleAction{
			Source: source, Playbook: playbook, Limit: limit,
		}})
}

func TestBuildPlaybookSpecResolvesPathSource(t *testing.T) {
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
		Name: "laptop-update", ActionKind: store.ActionPlaybook,
		Action: store.RuleAction{
			Source: "ops", Playbook: "plays/site.yml", Limit: "laptop",
			Tags: []string{"update", "deploy"}, Check: true, Diff: true,
		},
	})
	if err != nil {
		t.Fatalf("BuildPlaybookSpec: %v", err)
	}
	if spec.Kind != executor.KindAnsible {
		t.Errorf("kind = %q, want ansible", spec.Kind)
	}
	if spec.Source != "ops" {
		t.Errorf("source = %q, want ops", spec.Source)
	}
	if spec.Dir != checkout {
		t.Errorf("dir = %q, want the checkout %q", spec.Dir, checkout)
	}
	if !spec.DirReadOnly {
		t.Error("a default path source must be read-only")
	}
	if want := filepath.Join(checkout, "plays", "site.yml"); spec.Argv[len(spec.Argv)-1] != want {
		t.Errorf("argv playbook = %q, want the rebased path %q", spec.Argv[len(spec.Argv)-1], want)
	}
	joined := strings.Join(spec.Argv, " ")
	for _, want := range []string{"--limit laptop", "--tags update,deploy", "--check", "--diff"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q is missing %q", joined, want)
		}
	}
	// The engine stamps the identity fields after the builder returns; the
	// builder must leave them zero.
	if spec.Runbook != "" || spec.Trigger != "" || spec.DedupKey != "" ||
		spec.ConcurrencyKey != "" || spec.Timeout != 0 {
		t.Errorf("builder stamped identity fields: %+v", spec)
	}
}

func TestBuildPlaybookSpecUnknownSource(t *testing.T) {
	_, err := buildPlaybookSpec(t, service.New(openStore(t), t.TempDir(), nil), "ghost", "site.yml", "all")
	if err == nil {
		t.Fatal("an unknown source must be refused")
	}
	if !strings.Contains(err.Error(), "no source") {
		t.Errorf("error = %v, want it to name the missing source", err)
	}
}

func TestBuildPlaybookSpecRefusesEscapingPlaybook(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	checkout := t.TempDir()
	if _, err := st.CreateSource(ctx, store.Source{Name: "ops", Kind: store.SourcePath, URI: checkout, Enabled: true}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	svc := service.New(st, t.TempDir(), nil)

	outside := filepath.Join(t.TempDir(), "attacker.yml")
	if err := os.WriteFile(outside, []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write attacker playbook: %v", err)
	}
	for _, playbook := range []string{"/etc/passwd", outside, "nested/../../escape.yml"} {
		_, err := svc.BuildPlaybookSpec(ctx, reactive.Event{}, store.Rule{
			Name: "r", ActionKind: store.ActionPlaybook,
			Action: store.RuleAction{Source: "ops", Playbook: playbook, Limit: "all"},
		})
		if err == nil {
			t.Errorf("playbook %q resolved outside the checkout, want a refusal", playbook)
		}
	}
}

func TestBuildPlaybookSpecRefusesCheckoutHooks(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".exocomp", "hooks.yml"),
		[]byte("pre_run:\n  - name: ping\n    run: echo hi\n"), 0o600); err != nil {
		t.Fatalf("write hooks: %v", err)
	}
	if _, err := st.CreateSource(ctx, store.Source{Name: "ops", Kind: store.SourcePath, URI: checkout, Enabled: true}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	_, err := buildPlaybookSpec(t, service.New(st, t.TempDir(), nil), "ops", "site.yml", "all")
	if err == nil {
		t.Fatal("a checkout-hooked source must be refused")
	}
	if !strings.Contains(err.Error(), "hooks or a gate") {
		t.Errorf("error = %v, want it to name the hooks refusal", err)
	}
}

func TestBuildPlaybookSpecRefusesOverrideHooks(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	dataDir := t.TempDir()
	checkout := t.TempDir()
	if _, err := st.CreateSource(ctx, store.Source{Name: "ops", Kind: store.SourcePath, URI: checkout, Enabled: true}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	// The operator override under the data dir wins over the checkout's own
	// file; a source hooked only this way must still be refused.
	overrideDir := filepath.Join(dataDir, "hooks")
	if err := os.MkdirAll(overrideDir, 0o700); err != nil {
		t.Fatalf("mkdir override dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overrideDir, "ops.yml"),
		[]byte("pre_run:\n  - name: ping\n    run: echo hi\n"), 0o600); err != nil {
		t.Fatalf("write override: %v", err)
	}

	_, err := buildPlaybookSpec(t, service.New(st, dataDir, nil), "ops", "site.yml", "all")
	if err == nil {
		t.Fatal("a data-dir-override-hooked source must be refused")
	}
}

func TestBuildPlaybookSpecRequiresLimit(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	checkout := t.TempDir()
	if _, err := st.CreateSource(ctx, store.Source{Name: "ops", Kind: store.SourcePath, URI: checkout, Enabled: true}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	svc := service.New(st, t.TempDir(), nil)

	for _, action := range []store.RuleAction{
		{Source: "ops", Playbook: "site.yml"},                // no limit
		{Source: "ops", Playbook: "site.yml", Limit: "a\nb"}, // a limit ansible cannot read
		{Source: "ops", Playbook: "site.yml", Tags: []string{"x\x00y"}, Limit: "all"},
		{Playbook: "site.yml", Limit: "all"}, // no source
		{Source: "ops", Limit: "all"},        // no playbook
	} {
		_, err := svc.BuildPlaybookSpec(ctx, reactive.Event{}, store.Rule{
			Name: "r", ActionKind: store.ActionPlaybook, Action: action,
		})
		if err == nil {
			t.Errorf("action %+v built a spec, want a refusal", action)
		}
	}
}

// playbookSourceStore registers a path source with a real checkout and a
// playbook, returning the service under test.
func playbookSourceStore(t *testing.T) (*service.Service, *store.Store) {
	t.Helper()
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
	return service.New(st, t.TempDir(), nil), st
}
