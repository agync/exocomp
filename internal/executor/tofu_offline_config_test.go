package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// TestTofuOfflineProvidersWritesFilesystemMirrorConfig proves the driver
// wires TF_CLI_CONFIG_FILE to a filesystem-mirror-only configuration over
// PluginCacheDir whenever OfflineProviders is set. No "direct" method may be
// declared: tofu queries the registry's discovery document for any provider
// a declared "direct" method covers even when the mirror already has it,
// which would defeat offline resolution (see internal/executor/tofu.go).
func TestTofuOfflineProvidersWritesFilesystemMirrorConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	binary := filepath.Join(root, executor.TofuBinary)
	logPath := filepath.Join(root, "env.log")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  echo \"TF_CLI_CONFIG_FILE=${TF_CLI_CONFIG_FILE}\"\n" +
		"  echo \"TF_PLUGIN_CACHE_DIR=${TF_PLUGIN_CACHE_DIR}\"\n" +
		"  if [ -n \"$TF_CLI_CONFIG_FILE\" ]; then\n" +
		"    echo --config--\n" +
		"    cat \"$TF_CLI_CONFIG_FILE\"\n" +
		"  fi\n" +
		"} >> " + logPath + "\n" +
		"exit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	cache := t.TempDir()

	sink := &collector{}
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
	}, sink); err != nil {
		t.Fatalf("run: %v", err)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	out := string(log)
	if !strings.Contains(out, "TF_PLUGIN_CACHE_DIR="+cache) {
		t.Errorf("probe env missing TF_PLUGIN_CACHE_DIR=%s:\n%s", cache, out)
	}
	if !strings.Contains(out, "filesystem_mirror") || !strings.Contains(out, `path = "`+cache+`"`) {
		t.Errorf("offline config missing a filesystem_mirror over %s:\n%s", cache, out)
	}
	if strings.Contains(out, "direct") {
		t.Errorf("offline config declares a \"direct\" method, which defeats offline resolution:\n%s", out)
	}
}

// TestTofuOfflineProvidersRequiresPluginCacheDir proves the driver refuses
// to run rather than silently falling back to unrestricted registry access
// when OfflineProviders is set with nothing to resolve providers from.
func TestTofuOfflineProvidersRequiresPluginCacheDir(t *testing.T) {
	t.Parallel()
	sink := &collector{}
	_, err := executor.NewTofu(executor.TofuOptions{
		BackendURL:       "http://127.0.0.1:41111/api/v1/tf/state/edge",
		PrivateDirRoot:   t.TempDir(),
		OfflineProviders: true,
	}).Run(context.Background(), executor.Spec{
		Kind: executor.KindTofu,
		Argv: executor.TofuArgv("plan"),
		Dir:  t.TempDir(),
	}, sink)
	if err == nil {
		t.Fatal("Run succeeded with OfflineProviders set and no PluginCacheDir")
	}
}
