package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// fakeAnsibleEnvProbe puts a fake ansible-playbook on PATH that records its
// environment to a file and echoes it to stdout, returning the file's path.
func fakeAnsibleEnvProbe(t *testing.T) string {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "child.env")
	binDir := t.TempDir()
	script := "#!/bin/sh\nenv > " + probe + "\nenv\n"
	if err := os.WriteFile(filepath.Join(binDir, executor.AnsibleBinary), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ansible-playbook: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return probe
}

// TestAnsibleJobDriverInheritsSourceRunSecrets drives the real daemon driver:
// a job naming a source with a default binding and passing none reaches the
// child with the bound env, the row records the effective binding, and the
// value never reaches the row or the events. A job that names its own binding
// gets that one and not the source's.
func TestAnsibleJobDriverInheritsSourceRunSecrets(t *testing.T) {
	forceDisabledTier(t)
	dir := initForSource(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const sourceVal, ownVal = "source-default-secret", "own-binding-secret"
	bindVaultRunSecret(t, dir, "tree-vault", "tree-vault-pass", sourceVal)
	bindVaultRunSecret(t, dir, "own", "own-pass", ownVal)
	probe := fakeAnsibleEnvProbe(t)

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	src, err := st.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourceGit, Enabled: true})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := st.SetSourceRunSecrets(ctx, src.ID, "tree-vault"); err != nil {
		t.Fatalf("set default: %v", err)
	}
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()

	drv := ansibleJobDriver{store: st, env: env, dataDir: dir}
	q := queue.New(st, drv, queue.Options{Workers: 1, PollInterval: 20 * time.Millisecond})

	run := func(binding string) store.Job {
		t.Helper()
		job, err := q.Enqueue(ctx, queue.Spec{
			Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
			Argv:             executor.AnsibleArgv("/repo/site.yml", ""),
			Source:           "infra",
			RunSecretBinding: binding,
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := q.Drain(ctx); err != nil {
			t.Fatalf("drain: %v", err)
		}
		final, err := st.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if final.State != store.StateSucceeded {
			t.Fatalf("job state = %s, want succeeded", final.State)
		}
		return final
	}

	inherited := run("")
	if inherited.Args.RunSecretBinding != "tree-vault" {
		t.Errorf("row binding = %q, want tree-vault (the source default)", inherited.Args.RunSecretBinding)
	}
	childEnv, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	if !strings.Contains(string(childEnv), "VAULT_PASSWORD="+sourceVal) {
		t.Errorf("child env missing the source default's VAULT_PASSWORD:\n%s", childEnv)
	}
	events, err := st.ListJobEvents(ctx, inherited.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range events {
		if strings.Contains(e.Msg, sourceVal) {
			t.Errorf("secret value leaked into a job event: %q", e.Msg)
		}
	}

	explicit := run("own")
	if explicit.Args.RunSecretBinding != "own" {
		t.Errorf("row binding = %q, want own (explicit wins)", explicit.Args.RunSecretBinding)
	}
	childEnv, err = os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	if !strings.Contains(string(childEnv), "VAULT_PASSWORD="+ownVal) ||
		strings.Contains(string(childEnv), sourceVal) {
		t.Errorf("explicit run did not get only its own binding:\n%s", childEnv)
	}
}

// TestRunPlaybookInlineInheritsSourceRunSecrets covers the daemonless drain,
// which resolves the binding from the spec before the queue admits it: the
// source default must reach the child there too.
func TestRunPlaybookInlineInheritsSourceRunSecrets(t *testing.T) {
	forceDisabledTier(t)
	dir := initForSource(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const val = "inline-default-secret"
	bindVaultRunSecret(t, dir, "tree-vault", "tree-vault-pass", val)
	probe := fakeAnsibleEnvProbe(t)

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	src, err := st.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourceGit, Enabled: true})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := st.SetSourceRunSecrets(ctx, src.ID, "tree-vault"); err != nil {
		t.Fatalf("set default: %v", err)
	}

	var out, warn bytes.Buffer
	final, err := runPlaybookInline(ctx, st, dir, queue.Spec{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Argv:   executor.AnsibleArgv("/repo/site.yml", ""),
		Source: "infra",
	}, &out, &warn, nil)
	if err != nil {
		t.Fatalf("inline run: %v (%s)", err, warn.String())
	}
	if final.Args.RunSecretBinding != "tree-vault" {
		t.Errorf("row binding = %q, want tree-vault", final.Args.RunSecretBinding)
	}
	childEnv, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	if !strings.Contains(string(childEnv), "VAULT_PASSWORD="+val) {
		t.Errorf("child env missing the source default's VAULT_PASSWORD:\n%s", childEnv)
	}
	if strings.Contains(out.String(), val) {
		t.Errorf("secret value reached the streamed output:\n%s", out.String())
	}
}
