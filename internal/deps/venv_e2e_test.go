package deps_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/deps"
	"exocomp.dev/exocomp/internal/executor"
)

// TestVenvRunImportsDeclaredDep is the ADR 0011 gate exercised end to end with
// the real uv and real ansible-playbook: a project that declares a Python
// dependency runs inside its per-source venv and the dependency is importable
// under ansible_python_interpreter; a second run of the unchanged manifest
// invokes no uv; a changed manifest lands a new hash directory.
//
// It is gated behind EXOCOMP_DEPS_E2E because it invokes uv (which may reach the
// network on a cold cache) and runs ansible; make check stays hermetic.
func TestVenvRunImportsDeclaredDep(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if os.Getenv("EXOCOMP_DEPS_E2E") == "" {
		t.Skip("set EXOCOMP_DEPS_E2E=1 to run the uv+ansible end-to-end gate")
	}
	requireTool(t, "uv")
	requireTool(t, "ansible-playbook")

	cache := t.TempDir()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, "requirements.txt"), "jmespath==1.1.0\n")
	writeFile(t, filepath.Join(project, "site.yml"), importPlay)

	manifest, err := deps.DiscoverPython(project)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if manifest.Kind != deps.KindRequirements {
		t.Fatalf("kind = %q", manifest.Kind)
	}
	source := deps.SourceKey(manifest.Root)
	// UVCacheDir is granted write under the sandbox tier, where a Landlock rule
	// can only attach to a path that already exists — so EnsureVenv must create
	// it before invoking uv. Point it at a subdir that does not pre-exist.
	uvCache := filepath.Join(cache, "uv-cache")
	spec := deps.VenvSpec{CacheRoot: cache, Source: source, Manifest: manifest, UVCacheDir: uvCache}

	venv, err := deps.EnsureVenv(context.Background(), spec)
	if err != nil {
		t.Fatalf("ensure venv: %v", err)
	}
	if !venv.Created {
		t.Fatal("first ensure did not provision")
	}
	if fi, err := os.Stat(uvCache); err != nil || !fi.IsDir() {
		t.Fatalf("UVCacheDir not pre-created before uv ran: %v", err)
	}
	// The interpreter must live under cache/python/<source>/<hash>.
	wantPrefix := filepath.Join(cache, source, manifest.Hash)
	if !strings.HasPrefix(venv.Python, wantPrefix) {
		t.Fatalf("interpreter %s not under %s", venv.Python, wantPrefix)
	}

	// The dependency imports directly under the venv interpreter.
	out, err := exec.Command(venv.Python, "-c", "import jmespath; print(jmespath.__version__)").CombinedOutput()
	if err != nil {
		t.Fatalf("import under venv python: %v: %s", err, out)
	}
	t.Logf("venv python import jmespath -> %s", strings.TrimSpace(string(out)))

	// The full run: ansible drives a task that imports the dep, with the venv
	// forced as the interpreter.
	events := runPlay(t, venv, filepath.Join(project, "site.yml"))
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "JMESPATH_OK") {
		t.Fatalf("play did not import the dep at run time:\n%s", joined)
	}
	t.Logf("ansible run proved the dep importable under %s", venv.Python)

	// Second ensure of the unchanged manifest must not re-provision.
	again, err := deps.EnsureVenv(context.Background(), spec)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if again.Created {
		t.Error("unchanged manifest re-invoked uv")
	}

	// Changed manifest lands a new hash directory.
	writeFile(t, filepath.Join(project, "requirements.txt"), "jmespath==1.1.0\nnetaddr\n")
	changed, err := deps.DiscoverPython(project)
	if err != nil {
		t.Fatalf("discover changed: %v", err)
	}
	if changed.Hash == manifest.Hash {
		t.Fatal("hash did not change")
	}
	spec2 := spec
	spec2.Manifest = changed
	newVenv, err := deps.EnsureVenv(context.Background(), spec2)
	if err != nil {
		t.Fatalf("ensure changed: %v", err)
	}
	if newVenv.Dir == venv.Dir {
		t.Error("changed manifest reused the old hash dir")
	}
	t.Logf("unchanged manifest reused %s; changed manifest provisioned %s", venv.Dir, newVenv.Dir)
}

// importPlay runs on localhost and asserts jmespath imports under the forced
// interpreter, printing a sentinel the test greps for.
const importPlay = `- hosts: localhost
  connection: local
  gather_facts: false
  tasks:
    - name: import the declared dependency under the venv interpreter
      ansible.builtin.command: "{{ ansible_python_interpreter }} -c 'import jmespath; print(\"JMESPATH_OK\", jmespath.__version__)'"
      register: probe
    - name: surface it
      ansible.builtin.debug:
        msg: "{{ probe.stdout }}"
`

func runPlay(t *testing.T, venv deps.Venv, playbook string) []string {
	t.Helper()
	drv := executor.NewAnsible(executor.AnsibleOptions{
		Binary:      venv.Ansible,
		Interpreter: venv.Python,
		VenvBin:     venv.Bin,
	})
	sink := &collectSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := drv.Run(ctx, executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv(playbook, ""),
	}, sink)
	if err != nil {
		t.Fatalf("run play: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("play exited %d:\n%s", res.ExitCode, strings.Join(sink.lines(), "\n"))
	}
	return sink.lines()
}

type collectSink struct {
	mu   sync.Mutex
	msgs []string
}

func (c *collectSink) Emit(e executor.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, e.Msg)
}

func (c *collectSink) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.msgs))
	copy(out, c.msgs)
	return out
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH: %v", name, err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
