package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// envDumper writes a fake tool at binName that appends its whole environment,
// one "env:KEY=VALUE" line each, to a log, then exits 0.
func envDumper(t *testing.T, binName string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, binName)
	logPath = filepath.Join(dir, "env.log")
	script := "#!/bin/sh\nenv | sed 's/^/env:/' >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write %s: %v", binName, err)
	}
	return bin, logPath
}

func envLines(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// TestAnsibleRunSecretReachesChildNotParent is the credential-plane must-fix
// proof for ansible: a bound sealed secret arrives as named env, and the same
// name set in the daemon's own environment cannot leak in its place.
func TestAnsibleRunSecretReachesChild(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv("PROXMOX_PASSWORD", "leaked-from-parent")

	bin, logPath := envDumper(t, executor.AnsibleBinary)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var sink collector
	res, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:         bin,
		PrivateDirRoot: t.TempDir(),
		RunSecrets: executor.RunSecrets{
			"PROXMOX_PASSWORD": "bound-from-envelope",
			"PROXMOX_USER":     "root@pam",
		},
	}).Run(ctx, executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv("/p/site.yml", ""),
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}

	lines := envLines(t, logPath)
	if !slices.Contains(lines, "env:PROXMOX_PASSWORD=bound-from-envelope") {
		t.Errorf("bound secret did not reach the plugin: %v", lines)
	}
	if !slices.Contains(lines, "env:PROXMOX_USER=root@pam") {
		t.Errorf("bound user did not reach the plugin: %v", lines)
	}
	if slices.Contains(lines, "env:PROXMOX_PASSWORD=leaked-from-parent") {
		t.Errorf("the daemon's own PROXMOX_PASSWORD leaked into the run: %v", lines)
	}
}

// TestTofuRunSecretReachesChild is the twin proof for tofu: a bound provider
// token arrives as named env for the run, and a TF_VAR_ the daemon exported
// does not override a bound one.
func TestTofuRunSecretReachesChild(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "leaked-from-parent")
	t.Setenv("TF_VAR_region", "inherited-region")

	bin, logPath := envDumper(t, executor.TofuBinary)
	dir := t.TempDir()

	var sink collector
	res, err := executor.NewTofu(executor.TofuOptions{
		Binary:         bin,
		Workspace:      "edge",
		BackendURL:     "http://127.0.0.1:41111/api/v1/tf/state/edge",
		BackendToken:   "tok",
		PrivateDirRoot: t.TempDir(),
		RunSecrets: executor.RunSecrets{
			"CLOUDFLARE_API_TOKEN": "bound-from-envelope",
			"TF_VAR_region":        "bound-region",
		},
	}).Run(context.Background(), executor.Spec{
		JobID: 1,
		Kind:  executor.KindTofu,
		Argv:  executor.TofuArgv("plan"),
		Dir:   dir,
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}

	lines := envLines(t, logPath)
	if !slices.Contains(lines, "env:CLOUDFLARE_API_TOKEN=bound-from-envelope") {
		t.Errorf("bound provider token did not reach tofu: %v", lines)
	}
	if slices.Contains(lines, "env:CLOUDFLARE_API_TOKEN=leaked-from-parent") {
		t.Errorf("the daemon's own token leaked into the run: %v", lines)
	}
	// TF_VAR_ is inheritable, but a bound TF_VAR_ must win: it is appended last.
	if !slices.Contains(lines, "env:TF_VAR_region=bound-region") {
		t.Errorf("bound TF_VAR_region did not win over the inherited one: %v", lines)
	}
}

// TestAnsibleRunSecretBadNameFails proves the driver refuses to run rather than
// silently dropping a malformed binding.
func TestAnsibleRunSecretBadNameFails(t *testing.T) {
	t.Parallel()
	bin, _ := envDumper(t, executor.AnsibleBinary)
	_, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:         bin,
		PrivateDirRoot: t.TempDir(),
		RunSecrets:     executor.RunSecrets{"BAD NAME": "v"},
	}).Run(context.Background(), executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv("/p/site.yml", ""),
	}, &collector{})
	if err == nil {
		t.Fatal("run with a malformed env name should fail")
	}
}
