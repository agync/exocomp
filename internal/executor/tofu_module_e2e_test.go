package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestModuleCacheReuseWithRealTofu is the ADR 0011 tofu gate: a workspace with a
// git module resolves it once, and a second init through a fresh per-run
// TF_DATA_DIR whose modules directory is linked to the persistent cache reuses
// it — proven by deleting the module source before the second init, so any
// re-fetch would fail. It exercises linkModuleCache against the real tofu.
//
// Gated behind EXOCOMP_DEPS_E2E: it runs git and tofu; make check stays hermetic.
func TestModuleCacheReuseWithRealTofu(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real tofu")
	}
	if os.Getenv("EXOCOMP_DEPS_E2E") == "" {
		t.Skip("set EXOCOMP_DEPS_E2E=1 to run the tofu module-cache end-to-end gate")
	}
	for _, tool := range []string{"git", "tofu"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH: %v", tool, err)
		}
	}

	base := t.TempDir()
	cache := filepath.Join(base, "module-cache", "net")
	repo := filepath.Join(base, "greetrepo")
	ws := filepath.Join(base, "ws")
	mustMkdir(t, repo)
	mustMkdir(t, ws)

	// A git repo holding the module.
	writeF(t, filepath.Join(repo, "main.tf"), "output \"msg\" { value = \"hi\" }\n")
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	git(t, repo, "add", "main.tf")
	git(t, repo, "commit", "-qm", "init")

	writeF(t, filepath.Join(ws, "main.tf"),
		"module \"greet\" { source = \"git::file://"+repo+"\" }\n")

	tf := NewTofu(TofuOptions{ModuleCacheDir: cache})

	// First init populates the persistent cache.
	firstData := filepath.Join(base, "run1", "data")
	mustMkdir(t, firstData)
	if err := tf.linkModuleCache(firstData, SinkFunc(func(Event) {})); err != nil {
		t.Fatalf("link (first): %v", err)
	}
	if out, err := tofuInit(ws, firstData); err != nil {
		t.Fatalf("first init: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(cache, "modules.json")); err != nil {
		t.Fatalf("first init did not populate the cache: %v", err)
	}

	// Delete the source: a second fetch now must fail.
	if err := os.RemoveAll(repo); err != nil {
		t.Fatalf("remove repo: %v", err)
	}

	// Second init through a fresh data dir linked to the same cache.
	secondData := filepath.Join(base, "run2", "data")
	mustMkdir(t, secondData)
	if err := tf.linkModuleCache(secondData, SinkFunc(func(Event) {})); err != nil {
		t.Fatalf("link (second): %v", err)
	}
	out, err := tofuInit(ws, secondData)
	if err != nil {
		t.Fatalf("second init re-fetched a deleted module (no cache reuse): %v\n%s", err, out)
	}
	if strings.Contains(out, "Downloading") {
		t.Errorf("second init re-downloaded the module despite the cache:\n%s", out)
	}
	t.Logf("second init reused the cached module after source deletion:\n%s", out)
}

func tofuInit(dir, dataDir string) (string, error) {
	cmd := exec.Command("tofu", "init", "-no-color", "-input=false", "-backend=false")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TF_DATA_DIR="+dataDir, "TF_IN_AUTOMATION=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeF(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
