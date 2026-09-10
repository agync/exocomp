package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/secrets"
)

// TestWorkspaceAddStateOnlyEmitsBackendBlock proves mode A (ADR 0010): the
// emitted backend "http" block carries the workspace's state URL and its
// deterministic bearer, states the one-workspace-one-state rule, and — where
// tofu is on PATH — parses as valid HCL.
func TestWorkspaceAddStateOnlyEmitsBackendBlock(t *testing.T) {
	dir := initForSource(t)

	const addr = "127.0.0.1:12345"
	out, err := runCobra(t, newWorkspaceCmd, "add", "prod-net", "--state-only", "--address", addr)
	if err != nil {
		t.Fatalf("workspace add --state-only: %v (%s)", err, out)
	}

	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	wantToken := api.WorkspaceToken(env, "prod-net")
	_ = env.Close()
	wantURL := api.StateURL(addr, "prod-net")

	if !strings.Contains(out, "address        = \""+wantURL+"\"") {
		t.Errorf("emit missing state URL %q:\n%s", wantURL, out)
	}
	if strings.Contains(out, "Bearer "+wantToken) {
		t.Errorf("emit leaked the workspace bearer in the paste block:\n%s", out)
	}
	if !strings.Contains(out, "tofu init -backend-config=<(exocomp workspace backend prod-net --config)") {
		t.Errorf("emit missing the secret-free init line:\n%s", out)
	}
	if !strings.Contains(out, `lock_method    = "LOCK"`) ||
		!strings.Contains(out, `unlock_method  = "UNLOCK"`) {
		t.Errorf("emit missing LOCK/UNLOCK methods:\n%s", out)
	}
	if !strings.Contains(out, "one exocomp workspace") && !strings.Contains(out, "One exocomp workspace") {
		t.Errorf("emit does not state the one-workspace-one-state rule:\n%s", out)
	}
	if !strings.Contains(out, "workspace new") || !strings.Contains(out, "workspace select") {
		t.Errorf("emit does not state that tofu workspace new/select is unsupported:\n%s", out)
	}

	// The state-only emit now carries prose + the init command after the terraform
	// block; only the `terraform { ... }` section is HCL. Extract it so tofu fmt
	// validates the block alone.
	block, _, ok := strings.Cut(out, "# init (fills the bearer at init time, never on screen):")
	block = strings.TrimSuffix(strings.TrimSpace(block), "\n")
	if !ok {
		t.Fatal("emit missing the init-command delimiter")
	}
	assertParsesAsHCL(t, block)
}

// TestWorkspaceAddStateOnlyNeedsAddress proves the emit refuses when it has no
// address to target rather than printing an unusable block.
func TestWorkspaceAddStateOnlyNeedsAddress(t *testing.T) {
	initForSource(t)
	if _, err := runCobra(t, newWorkspaceCmd, "add", "no-addr", "--state-only"); err == nil {
		t.Fatal("expected --state-only with no recorded address and no --address to fail")
	}
}

// assertParsesAsHCL proves the emitted block is valid, canonically formatted
// HCL an unmodified tofu accepts: `tofu fmt -check` exits 0 only when the file
// both parses and is already canonical. Skipped (not failed) when tofu is not
// on PATH, so the suite stays hermetic.
func assertParsesAsHCL(t *testing.T, block string) {
	t.Helper()
	tofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Skip("tofu not on PATH; skipping HCL parse check")
	}
	f := filepath.Join(t.TempDir(), "backend.tf")
	if err := os.WriteFile(f, []byte(block), 0o644); err != nil {
		t.Fatalf("write block: %v", err)
	}
	if outb, ferr := exec.Command(tofu, "fmt", "-check", f).CombinedOutput(); ferr != nil {
		t.Fatalf("tofu rejected the emitted block: %v\n%s", ferr, outb)
	}
}
