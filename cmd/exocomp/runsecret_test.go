package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// forceDisabledTier pins the job tier to disabled for one test, so a run does
// not depend on nono/Landlock being enforceable in the test environment. The
// sandbox decision is a process global memoized behind a sync.Once; the test
// resets it and restores it so a later test re-probes.
func forceDisabledTier(t *testing.T) {
	t.Helper()
	sandbox.once = sync.Once{}
	sandbox.requested = executor.TierDisabled
	t.Cleanup(func() {
		sandbox.once = sync.Once{}
		sandbox.requested = ""
	})
}

// TestResolveRunSecretsBindingAndInventory proves the resolver turns a named
// run_secret binding and a dynamic inventory's env_binding into the run's env,
// decrypting each sealed value through the envelope, and returns the plaintexts
// for the redactor.
func TestResolveRunSecretsBindingAndInventory(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	sealSecretDirect(t, dir, "prox-token", "super-secret-token")
	sealSecretDirect(t, dir, "pve-url", "https://pve.example:8006")

	if out, err := runRunSecret(t, "add", "prox", "PROXMOX_PASSWORD", "prox-token"); err != nil {
		t.Fatalf("run-secret add: %v (%s)", err, out)
	}
	if out, err := runInventory(t, "add", "pve", "--kind", "dynamic",
		"--path", "inventory/pve.yml", "--bind", "PROXMOX_URL=pve-url"); err != nil {
		t.Fatalf("inventory add: %v (%s)", err, out)
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()

	rs, plain, err := resolveRunSecrets(ctx, st, env, "prox", "pve")
	if err != nil {
		t.Fatalf("resolveRunSecrets: %v", err)
	}
	if rs["PROXMOX_PASSWORD"] != "super-secret-token" {
		t.Errorf("PROXMOX_PASSWORD = %q, want the sealed value", rs["PROXMOX_PASSWORD"])
	}
	if rs["PROXMOX_URL"] != "https://pve.example:8006" {
		t.Errorf("PROXMOX_URL = %q, want the inventory-bound value", rs["PROXMOX_URL"])
	}
	if len(plain) != 2 {
		t.Errorf("plaintexts = %d, want 2 (both scrubbed by the redactor)", len(plain))
	}

	// A binding that resolves nothing is an error, not a silent empty env.
	if _, _, err := resolveRunSecrets(ctx, st, env, "missing", ""); err == nil {
		t.Error("expected an unknown binding to be refused")
	}
}

// TestResolveRunSecretsAnsibleOutputBinding proves the tofu step of a bootstrap
// chain inherits its play's sealed captures as env through the ab:<workflow>
// binding, empty-tolerantly: a workflow with a bound capture resolves it, and a
// workflow whose binding is empty resolves to no env without the error a normal
// explicit binding raises. This is the consumer half of the ansible->tofu loop.
func TestResolveRunSecretsAnsibleOutputBinding(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	const kube = "KUBECONFIG-SENTINEL-9f3a1c-so-length-is-distinctive"
	sealSecretDirect(t, dir, executor.AnsibleOutputSecretName("wf1", "KUBECONFIG"), kube)
	if out, err := runRunSecret(t, "add", executor.AnsibleOutputBinding("wf1"),
		"KUBECONFIG", executor.AnsibleOutputSecretName("wf1", "KUBECONFIG")); err != nil {
		t.Fatalf("run-secret add: %v (%s)", err, out)
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()

	// This workflow's tofu step names ab:wf1 and gets the sealed value as env.
	rs, plain, err := resolveRunSecrets(ctx, st, env, executor.AnsibleOutputBinding("wf1"), "")
	if err != nil {
		t.Fatalf("resolveRunSecrets(ab:wf1): %v", err)
	}
	if rs["KUBECONFIG"] != kube {
		t.Errorf("KUBECONFIG = %q, want the sealed capture", rs["KUBECONFIG"])
	}
	if len(plain) != 1 {
		t.Errorf("plaintexts = %d, want 1 (scrubbed by the redactor)", len(plain))
	}

	// An unrelated workflow's binding is empty and MUST resolve to no env rather
	// than the ErrNotFound a normal explicit binding raises: a Phase-0 chain, or a
	// chain whose captures did not bind, must not fail its apply.
	other, _, err := resolveRunSecrets(ctx, st, env, executor.AnsibleOutputBinding("wf-unrelated"), "")
	if err != nil {
		t.Fatalf("an empty ab: binding must be tolerated, got: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("an unrelated workflow inherited env %v, want none", other)
	}
}

// TestAnsibleJobDriverBoundSecretReachesChildRedactedAndOffRow is the item-1
// end-to-end proof through the real daemon driver: a job that names a
// run_secret binding and a dynamic inventory gets both values as env in the
// child process, while the job row carries only the binding names and the
// events never carry the secret values.
func TestAnsibleJobDriverBoundSecretReachesChildRedactedAndOffRow(t *testing.T) {
	forceDisabledTier(t)
	dir := initForSource(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const tokenVal = "super-secret-token-value"
	const urlVal = "https://pve.example:8006"
	sealSecretDirect(t, dir, "prox-token", tokenVal)
	sealSecretDirect(t, dir, "pve-url", urlVal)
	if _, err := runRunSecret(t, "add", "prox", "PROXMOX_PASSWORD", "prox-token"); err != nil {
		t.Fatalf("run-secret add: %v", err)
	}
	if _, err := runInventory(t, "add", "pve", "--kind", "dynamic",
		"--path", "inventory/pve.yml", "--bind", "PROXMOX_URL=pve-url"); err != nil {
		t.Fatalf("inventory add: %v", err)
	}

	// A fake ansible-playbook that records its whole environment two ways: to a
	// probe file read straight off disk (proving the value reached the child),
	// and to stdout that flows through the run's redactor (proving redaction).
	probe := filepath.Join(t.TempDir(), "child.env")
	binDir := t.TempDir()
	script := "#!/bin/sh\nenv > " + probe + "\nenv\n"
	if err := os.WriteFile(filepath.Join(binDir, executor.AnsibleBinary), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ansible-playbook: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()

	// The queue carries NO general redactor, so the only thing that can scrub
	// the bound value from events is the per-job runSink this wiring builds.
	drv := ansibleJobDriver{store: st, env: env, dataDir: dir}
	q := queue.New(st, drv, queue.Options{Workers: 1, PollInterval: 20 * time.Millisecond})

	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook:          "playbook",
		Kind:             executor.KindAnsible,
		Trigger:          "manual",
		Argv:             executor.AnsibleArgv("/repo/site.yml", "inventory/pve.yml"),
		RunSecretBinding: "prox",
		Inventory:        "pve",
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

	// (b) the bound values reached the child's environment.
	childEnv, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	if !strings.Contains(string(childEnv), "PROXMOX_PASSWORD="+tokenVal) {
		t.Errorf("child env missing bound PROXMOX_PASSWORD:\n%s", childEnv)
	}
	if !strings.Contains(string(childEnv), "PROXMOX_URL="+urlVal) {
		t.Errorf("child env missing inventory-bound PROXMOX_URL:\n%s", childEnv)
	}

	// (c) the job row carries only the binding names, never the secret values.
	rowArgs, err := json.Marshal(final.Args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if strings.Contains(string(rowArgs), tokenVal) || strings.Contains(string(rowArgs), urlVal) {
		t.Errorf("secret value leaked into the job row: %s", rowArgs)
	}
	if !strings.Contains(string(rowArgs), "prox") || !strings.Contains(string(rowArgs), "pve") {
		t.Errorf("job row missing the binding/inventory names: %s", rowArgs)
	}

	// (d) the events echoed the child's env but the redactor scrubbed the value.
	events, err := st.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	sawEnvDump := false
	for _, e := range events {
		if strings.Contains(e.Msg, "PROXMOX_PASSWORD=") {
			sawEnvDump = true
		}
		if strings.Contains(e.Msg, tokenVal) || strings.Contains(e.Msg, urlVal) {
			t.Errorf("secret value leaked into a job event: %q", e.Msg)
		}
	}
	if !sawEnvDump {
		t.Error("expected the child's env dump in events (proving the redaction path ran over it)")
	}
}
