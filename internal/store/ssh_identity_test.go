package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestJobArgsSSHIdentityFilesRoundTrip: an ansible job's declared ssh identity
// files ride on the job row as ssh_identity_files, surviving a
// marshal/unmarshal so a daemon that claims the row rebuilds the same grant.
// Empty stays omitted, so an ordinary job row reads exactly as before.
func TestJobArgsSSHIdentityFilesRoundTrip(t *testing.T) {
	t.Parallel()

	in := store.JobArgs{Argv: []string{"ansible-playbook", "site.yml"},
		SSHIdentityFiles: []string{"/keys/lima", "/keys/prod"}}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !slices.Contains(bytesFields(t, body), "ssh_identity_files") {
		t.Fatalf("ssh_identity_files missing from %s", body)
	}
	var out store.JobArgs
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !slices.Equal(out.SSHIdentityFiles, []string{"/keys/lima", "/keys/prod"}) {
		t.Fatalf("round-trip SSHIdentityFiles = %v, want [/keys/lima /keys/prod]", out.SSHIdentityFiles)
	}

	plain, err := json.Marshal(store.JobArgs{Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	if slices.Contains(bytesFields(t, plain), "ssh_identity_files") {
		t.Fatalf("an empty identity set must stay omitted: %s", plain)
	}
}

// TestJobSSHIdentityFilesSurviveTheStore: the row the queue writes and a daemon
// reads back carries the declared identity files, the same path a re-run
// clones.
func TestJobSSHIdentityFilesSurviveTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := open(t)

	job, err := s.CreateJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: "ansible", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"ansible-playbook", "site.yml"},
			SSHIdentityFiles: []string{"/keys/lima"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	got, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(got.Args.SSHIdentityFiles, []string{"/keys/lima"}) {
		t.Fatalf("stored SSHIdentityFiles = %v, want [/keys/lima]", got.Args.SSHIdentityFiles)
	}
}

// TestRuleActionSSHIdentityFilesRoundTrip: a playbook-action rule's declared
// identity files ride in the action as ssh_identity_files, so the reactive
// builder reads the same grant back when the rule fires. Empty stays omitted.
func TestRuleActionSSHIdentityFilesRoundTrip(t *testing.T) {
	t.Parallel()

	in := store.RuleAction{Source: "ops", Playbook: "site.yml", Limit: "laptop",
		SSHIdentityFiles: []string{"/keys/lima"}}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !slices.Contains(bytesFields(t, body), "ssh_identity_files") {
		t.Fatalf("ssh_identity_files missing from %s", body)
	}
	var out store.RuleAction
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !slices.Equal(out.SSHIdentityFiles, []string{"/keys/lima"}) {
		t.Fatalf("round-trip SSHIdentityFiles = %v, want [/keys/lima]", out.SSHIdentityFiles)
	}

	plain, err := json.Marshal(store.RuleAction{Source: "ops"})
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	if slices.Contains(bytesFields(t, plain), "ssh_identity_files") {
		t.Fatalf("an empty identity set must stay omitted: %s", plain)
	}
}

// TestRuleSSHIdentityFilesSurviveTheStore: the rule row the engine loads
// carries the declared identity files when a match fires.
func TestRuleSSHIdentityFilesSurviveTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := open(t)

	created, err := s.CreateRule(ctx, store.NewRule{
		Name: "r", CELExpr: "true", ActionKind: store.ActionPlaybook,
		Action: store.RuleAction{Source: "ops", Playbook: "site.yml", Limit: "laptop",
			SSHIdentityFiles: []string{"/keys/lima"}},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	got, err := s.GetRule(ctx, created.Name)
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if !slices.Equal(got.Action.SSHIdentityFiles, []string{"/keys/lima"}) {
		t.Fatalf("stored SSHIdentityFiles = %v, want [/keys/lima]", got.Action.SSHIdentityFiles)
	}
}

// TestNormalizeSSHIdentityFiles: the exported seam every write face validates
// through sorts, de-duplicates, drops empties, and refuses a relative path — a
// grant the sandbox would refuse at build time is surfaced when declared.
// Widening the absolute-path check in normalizeSSHIdentityFiles fails this
// test.
func TestNormalizeSSHIdentityFiles(t *testing.T) {
	t.Parallel()

	clean, err := store.NormalizeSSHIdentityFiles([]string{"/keys/b", "/keys/a", "/keys/b"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if want := []string{"/keys/a", "/keys/b"}; !slices.Equal(clean, want) {
		t.Fatalf("NormalizeSSHIdentityFiles = %v, want %v (sorted, deduped)", clean, want)
	}

	for _, bad := range []string{"keys/lima", "~/.ssh/id", ".", "run/key"} {
		if _, err := store.NormalizeSSHIdentityFiles([]string{bad}); !errors.Is(err, store.ErrInvalidSSHIdentity) {
			t.Errorf("NormalizeSSHIdentityFiles(%q) err = %v, want ErrInvalidSSHIdentity", bad, err)
		}
	}
	if _, err := store.NormalizeSSHIdentityFiles([]string{"/keys/a", "rel"}); !errors.Is(err, store.ErrInvalidSSHIdentity) {
		t.Errorf("mixed set err = %v, want ErrInvalidSSHIdentity (no partial accept)", err)
	}
}
