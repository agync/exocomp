package executor_test

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// sandboxDataDir stands up a data dir holding the files ADR 0005 says a job
// must never reach, so a run that reaches one fails visibly.
func sandboxDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"host.key":    "AGE-SECRET-KEY-NOT-A-REAL-ONE\n",
		"exocomp.db":  "SQLite format 3\x00",
		"keyring.age": "age-encryption.org/v1\n",
		"serve.lock":  "",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// writePlaybook drops a playbook into dir and returns its path.
func writePlaybook(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestSandboxedAnsibleRunsANormalPlaybook: the sandbox is only worth having if
// ordinary work still runs under it, so this is a real ansible-playbook run
// reading its own repository checkout through the local connection.
func TestSandboxedAnsibleRunsANormalPlaybook(t *testing.T) {
	requireNono(t)
	requireAnsible(t)

	data := sandboxDataDir(t)
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "fact.txt"), []byte("checkout-readable\n"), 0o600); err != nil {
		t.Fatalf("write fact: %v", err)
	}
	book := writePlaybook(t, repo, "site.yml", `
- name: sandboxed smoke
  hosts: localhost
  gather_facts: false
  tasks:
    - name: read a file from the checkout
      ansible.builtin.command: cat {{ playbook_dir }}/fact.txt
      changed_when: false
      register: fact
    - name: report it
      ansible.builtin.debug:
        msg: "{{ fact.stdout }}"
`)

	res, sink := runSandboxedAnsible(t, data, book)
	if res.ExitCode != 0 {
		t.Fatalf("playbook exited %d under the sandbox:\n%s",
			res.ExitCode, strings.Join(sink.msgs(""), "\n"))
	}
	if log := strings.Join(sink.msgs(""), "\n"); !strings.Contains(log, "checkout-readable") {
		t.Errorf("playbook did not read its checkout:\n%s", log)
	}
}

// TestSandboxedAnsibleCannotReachTheDataDir is the gate ADR 0005 states: a
// playbook is arbitrary code from a repository, and under this tier it can no
// longer read the database or the keys. The same playbook under the plain
// local tier reads every one of them, which is what makes this a test of the
// sandbox rather than of the filesystem.
func TestSandboxedAnsibleCannotReachTheDataDir(t *testing.T) {
	requireNono(t)
	requireAnsible(t)

	data := sandboxDataDir(t)
	repo := t.TempDir()
	book := writePlaybook(t, repo, "steal.yml", `
- name: reach for the control plane's own state
  hosts: localhost
  gather_facts: false
  tasks:
    - name: read every protected file
      ansible.builtin.command: cat {{ item }}
      changed_when: false
      loop:
        - `+filepath.Join(data, "host.key")+`
        - `+filepath.Join(data, "exocomp.db")+`
        - `+filepath.Join(data, "keyring.age")+`
        - `+filepath.Join(data, "serve.lock")+`
`)

	sandboxed, sandboxSink := runSandboxedAnsible(t, data, book)
	if sandboxed.ExitCode == 0 {
		t.Fatalf("a sandboxed playbook read the data dir:\n%s",
			strings.Join(sandboxSink.msgs(""), "\n"))
	}
	if log := strings.Join(sandboxSink.msgs(""), "\n"); !strings.Contains(log, "Permission denied") {
		t.Errorf("the run failed for some reason other than the sandbox:\n%s", log)
	}

	plain, plainSink := runPlainAnsible(t, book)
	if plain.ExitCode != 0 {
		t.Fatalf("the same playbook failed without the sandbox, so the test proves nothing:\n%s",
			strings.Join(plainSink.msgs(""), "\n"))
	}
}

func runSandboxedAnsible(t *testing.T, dataDir, playbook string) (executor.Result, *collector) {
	t.Helper()
	return runAnsibleDriver(t, executor.NewSandboxedAnsible(
		executor.AnsibleOptions{PrivateDirRoot: shortPrivateRoot(t)},
		executor.SandboxOptions{DataDir: dataDir},
	), playbook)
}

func runPlainAnsible(t *testing.T, playbook string) (executor.Result, *collector) {
	t.Helper()
	return runAnsibleDriver(t, executor.NewAnsible(
		executor.AnsibleOptions{PrivateDirRoot: shortPrivateRoot(t)}), playbook)
}

// shortPrivateRoot stands in for the data dir's run directory. t.TempDir()
// embeds the test's name, and a run whose TMPDIR sits under it overruns the
// 108-byte AF_UNIX limit that ansible-core's RPC host and OpenTofu's plugin
// handshake both live inside — a deployment path like /var/lib/exocomp/run
// never comes close.
func shortPrivateRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "exo")
	if err != nil {
		t.Fatalf("private root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func runAnsibleDriver(t *testing.T, drv executor.Driver, playbook string) (executor.Result, *collector) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var sink collector
	res, err := drv.Run(ctx, executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv(playbook, ""),
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res, &sink
}

// TestSandboxedTofuPlansAndApplies: a tofu run under the sandbox still reaches
// the loopback state backend and the provider registry, because the profile
// grants exactly those two ports and nothing else.
func TestSandboxedTofuPlansAndApplies(t *testing.T) {
	requireNono(t)
	requireTofu(t)

	data := sandboxDataDir(t)
	tofuDir := t.TempDir()
	config := "resource \"local_file\" \"note\" {\n" +
		"  filename = \"${path.module}/note.txt\"\n" +
		"  content  = \"sandboxed-tofu-ok\\n\"\n}\n"
	if err := os.WriteFile(filepath.Join(tofuDir, "main.tf"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	st, err := store.Open(filepath.Join(data, "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	env, _, err := secrets.Init(filepath.Join(data, "envelope"), nil)
	if err != nil {
		t.Fatalf("init envelope: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })

	ws, err := st.CreateWorkspace(context.Background(), store.Workspace{Name: "sandboxed", TofuDir: tofuDir})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	srv, err := api.New(api.Options{Store: st, Tokens: env, Port: 0})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start backend: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown backend: %v", err)
		}
	})

	job, err := st.CreateJob(context.Background(), store.NewJob{
		Runbook: "workspace:" + ws.Name,
		Kind:    executor.KindTofu,
		Trigger: "test",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	srv.BindJob(ws.Name, job.ID)
	t.Cleanup(func() { srv.ReleaseJob(ws.Name) })

	stateKey := env.Derive(executor.TofuStatePurpose, ws.Name)
	drv := executor.NewSandboxedTofu(executor.TofuOptions{
		Workspace:       ws.Name,
		BackendURL:      api.StateURL(srv.Addr(), ws.Name),
		BackendToken:    api.WorkspaceToken(env, ws.Name),
		StatePassphrase: base64.StdEncoding.EncodeToString(stateKey),
		PrivateDirRoot:  shortPrivateRoot(t),
		PluginCacheDir:  tofuPluginCache(t),
	}, executor.SandboxOptions{DataDir: data})

	for _, op := range [][]string{{"plan"}, {"apply", "-auto-approve"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		var sink collector
		res, err := drv.Run(ctx, executor.Spec{
			JobID: job.ID,
			Kind:  executor.KindTofu,
			Argv:  executor.TofuArgv(op[0], op[1:]...),
			Dir:   tofuDir,
		}, &sink)
		cancel()
		if err != nil {
			t.Fatalf("%v: %v\n%s", op, err, strings.Join(sink.msgs(""), "\n"))
		}
		if res.ExitCode != 0 {
			t.Fatalf("%v exited %d under the sandbox:\n%s",
				op, res.ExitCode, strings.Join(sink.msgs(""), "\n"))
		}
	}

	body, err := os.ReadFile(filepath.Join(tofuDir, "note.txt"))
	if err != nil {
		t.Fatalf("apply wrote nothing: %v", err)
	}
	if strings.TrimSpace(string(body)) != "sandboxed-tofu-ok" {
		t.Errorf("managed file holds %q", body)
	}
}
