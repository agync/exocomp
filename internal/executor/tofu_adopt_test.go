package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// TestTofuAdoptionConfigWidensEncryption proves the adoption run's TF_ENCRYPTION
// document carries the unencrypted fallback method while a normal run stays
// aes_gcm-only, and that an adoption init passes -force-copy (which implies
// -migrate-state and clears the interactive confirmation -input=false would
// block) so tofu migrates pre-existing unencrypted local state onto the HTTP
// backend instead of refusing the backend change.
func TestTofuAdoptionConfigWidensEncryption(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	binary := filepath.Join(root, executor.TofuBinary)
	logPath := filepath.Join(root, "env.log")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  echo \"TF_ENCRYPTION=${TF_ENCRYPTION}\"\n" +
		"  echo \"ARGV=$*\"\n" +
		"} >> " + logPath + "\n" +
		"exit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	sink := &collector{}
	dir := t.TempDir()
	workspace := filepath.Join(dir, "ws")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	// A normal run: aes_gcm only, no -migrate-state.
	if _, err := executor.NewTofu(executor.TofuOptions{
		Binary:          binary,
		BackendURL:      "http://127.0.0.1:41111/api/v1/tf/state/edge",
		StatePassphrase: "c2Vlc2Vlc2Vlc2Vlc2Vlc2Vl",
		PrivateDirRoot:  t.TempDir(),
	}).Run(context.Background(), executor.Spec{
		Kind: executor.KindTofu,
		Argv: executor.TofuArgv("plan"),
		Dir:  workspace,
	}, sink); err != nil {
		t.Fatalf("normal run: %v", err)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	normal := string(body)
	if !strings.Contains(normal, "method \"aes_gcm\" \"exocomp\"") {
		t.Fatalf("normal TF_ENCRYPTION missing aes_gcm method:\n%v", normal)
	}
	if strings.Contains(normal, "unencrypted") {
		t.Fatalf("normal run must NOT carry the unencrypted fallback:\n%v", normal)
	}
	if strings.Contains(normal, "-migrate-state") {
		t.Fatalf("normal run must not pass -migrate-state:\n%v", normal)
	}

	// An adoption run: the unencrypted fallback method and -migrate-state.
	adoptLog := filepath.Join(root, "adopt.log")
	adoptScript := "#!/bin/sh\n" +
		"{\n" +
		"  echo \"TF_ENCRYPTION=${TF_ENCRYPTION}\"\n" +
		"  echo \"ARGV=$*\"\n" +
		"} >> " + adoptLog + "\n" +
		"exit 0\n"
	if err := os.WriteFile(binary, []byte(adoptScript), 0o755); err != nil {
		t.Fatalf("write adopt probe: %v", err)
	}
	adoptSink := &collector{}
	if _, err := executor.NewTofu(executor.TofuOptions{
		Binary:          binary,
		BackendURL:      "http://127.0.0.1:41111/api/v1/tf/state/edge",
		StatePassphrase: "c2Vlc2Vlc2Vlc2Vlc2Vlc2Vl",
		PrivateDirRoot:  t.TempDir(),
		AdoptState:      true,
	}).Run(context.Background(), executor.Spec{
		Kind: executor.KindTofu,
		Argv: executor.TofuArgv("plan"),
		Dir:  workspace,
	}, adoptSink); err != nil {
		t.Fatalf("adopt run: %v", err)
	}
	adoptBody, err := os.ReadFile(adoptLog)
	if err != nil {
		t.Fatalf("read adopt log: %v", err)
	}
	adopt := string(adoptBody)
	if !strings.Contains(adopt, "method \"unencrypted\" \"exocomp\"") {
		t.Fatalf("adopt TF_ENCRYPTION missing unencrypted method:\n%v", adopt)
	}
	if !strings.Contains(adopt, "fallback {") || !strings.Contains(adopt, "method.unencrypted.exocomp") {
		t.Fatalf("adopt TF_ENCRYPTION missing the unencrypted fallback wiring:\n%v", adopt)
	}
	if !strings.Contains(adopt, "-force-copy") {
		t.Fatalf("adopt init must pass -force-copy (implies -migrate-state):\n%v", adopt)
	}
}
