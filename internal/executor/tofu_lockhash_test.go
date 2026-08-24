package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// TestTofuOfflineLocksProvidersBeforeInit proves the offline path regenerates
// the provider lock from the filesystem mirror for the running platform before
// it inits: a `tofu providers lock -fs-mirror <cache> -platform=<os_arch>`
// runs, and it runs before `init`, so the offline init
// verifies against h1: hashes the mirror can satisfy rather than the zh: hashes
// an online lock would carry. Hermetic: a fake tofu records each invocation.
func TestTofuOfflineLocksProvidersBeforeInit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	binary := filepath.Join(root, executor.TofuBinary)
	logPath := filepath.Join(root, "argv.log")
	// Each invocation appends its full argv as one line.
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tofu: %v", err)
	}
	cache := t.TempDir()

	if _, err := executor.NewTofu(executor.TofuOptions{
		Binary:           binary,
		BackendURL:       "http://127.0.0.1:41111/api/v1/tf/state/edge",
		PrivateDirRoot:   t.TempDir(),
		PluginCacheDir:   cache,
		OfflineProviders: true,
	}).Run(context.Background(), executor.Spec{
		Kind: executor.KindTofu,
		Argv: executor.TofuArgv("plan"),
		Dir:  t.TempDir(),
	}, &collector{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")

	lockIdx, initIdx := -1, -1
	platform := runtime.GOOS + "_" + runtime.GOARCH
	for i, line := range lines {
		if strings.Contains(line, "providers lock") {
			lockIdx = i
			if !strings.Contains(line, "-fs-mirror="+cache) {
				t.Errorf("providers lock missing -fs-mirror=%s: %q", cache, line)
			}
			if !strings.Contains(line, "-platform="+platform) {
				t.Errorf("providers lock missing -platform=%s: %q", platform, line)
			}
		}
		if initIdx == -1 && strings.HasPrefix(line, "init ") {
			initIdx = i
		}
	}
	if lockIdx == -1 {
		t.Fatalf("offline run never ran `providers lock`:\n%s", log)
	}
	if initIdx == -1 {
		t.Fatalf("offline run never ran `init`:\n%s", log)
	}
	if lockIdx > initIdx {
		t.Errorf("providers lock ran after init (%d > %d); it must precede it:\n%s", lockIdx, initIdx, log)
	}
}
