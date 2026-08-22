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

// forceLocalTier pins the job tier to local for one test, so a run does not
// depend on nono/Landlock being enforceable in the test environment. The
// sandbox decision is a process global memoized behind a sync.Once; the test
// resets it and restores it so a later test re-probes.
func forceLocalTier(t *testing.T) {
	t.Helper()
	sandbox.once = sync.Once{}
	sandbox.requested = executor.TierLocal
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

// TestAnsibleJobDriverBoundSecretReachesChildRedactedAndOffRow is the item-1
// end-to-end proof through the real daemon driver: a job that names a
// run_secret binding and a dynamic inventory gets both values as env in the
// child process, while the job row carries only the binding names and the
// events never carry the secret values.
func TestAnsibleJobDriverBoundSecretReachesChildRedactedAndOffRow(t *testing.T) {
	forceLocalTier(t)
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
