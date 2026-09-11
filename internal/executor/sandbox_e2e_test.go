package executor_test

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	if testing.Short() {
		t.Skip("requires real ansible")
	}
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
	if testing.Short() {
		t.Skip("requires real ansible")
	}
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
	if testing.Short() {
		t.Skip("requires real tofu")
	}
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

// devShmSecretSentinel is the plaintext staged under /dev/shm by the tests
// below — never checked into an event or an argv, only into the file a
// sandboxed child reads back.
const devShmSecretSentinel = "apiVersion: v1\nkind: Config\ntoken: sandboxed-devshm-bearer\n"

// TestSandboxedTofuReadsDevShmStagedSecret is the win this rework exists for:
// under real nono, with zero added capabilities, a sandboxed tofu child reads
// a run-secret staged as a file under /dev/shm through the path granted to
// it — the exact case the rejected memfd design could not support, because
// real nono closes inherited fds before exec and a memfd has no path a
// --read-file grant can name.
func TestSandboxedTofuReadsDevShmStagedSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	requireNono(t)

	data := sandboxDataDir(t)
	// bin and its log both live inside the working directory the sandbox
	// grants read+write on (Sandbox.Run adds spec.Dir to WritePaths); the
	// tool's own directory is otherwise granted read-only, which would deny
	// the script's own `>> log` append under the sandbox.
	workDir := t.TempDir()
	bin, logPath := fileStagerBin(t, executor.TofuBinary, workDir)

	drv := executor.NewSandboxedTofu(executor.TofuOptions{
		Binary:         bin,
		Workspace:      "edge",
		BackendURL:     "http://127.0.0.1:41111/api/v1/tf/state/edge",
		BackendToken:   "tok",
		PrivateDirRoot: shortPrivateRoot(t),
		RunSecrets:     executor.RunSecrets{"KUBECONFIG": devShmSecretSentinel},
		RunSecretFiles: []string{"KUBECONFIG"},
	}, executor.SandboxOptions{DataDir: data})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var sink collector
	res, err := drv.Run(ctx, executor.Spec{
		JobID: 1,
		Kind:  executor.KindTofu,
		Argv:  executor.TofuArgv("plan"),
		Dir:   workDir,
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("sandboxed tofu exited %d reading its granted /dev/shm secret:\n%s",
			res.ExitCode, strings.Join(sink.msgs(""), "\n"))
	}

	lines := envLines(t, logPath)
	var kubePath string
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "env:KUBECONFIG="); ok {
			kubePath = v
			break
		}
	}
	if kubePath == "" {
		t.Fatalf("KUBECONFIG did not reach the sandboxed child: %v", lines)
	}
	if !strings.HasPrefix(kubePath, executor.TmpfsRoot+"/") {
		t.Fatalf("staged path %q is not under %s", kubePath, executor.TmpfsRoot)
	}
	// The sandboxed child actually opened the granted file — not just saw the
	// path — and read the sentinel back at 0600.
	if !slices.Contains(lines, "kubeconfig-mode:600") {
		t.Errorf("sandboxed child did not read a 0600 file: %v", lines)
	}
	wantBody := "kubeconfig-body:" + strings.ReplaceAll(devShmSecretSentinel, "\n", "~")
	if !slices.Contains(lines, wantBody) {
		t.Errorf("sandboxed child did not read the sentinel: %v", lines)
	}
	for _, e := range sink.events {
		if strings.Contains(e.Msg, "sandboxed-devshm-bearer") || strings.Contains(e.RawJSON, "sandboxed-devshm-bearer") {
			t.Errorf("the sentinel value reached a job event: %+v", e)
		}
	}

	// The job is over: the staged file was unlinked once the sandboxed
	// subprocess exited.
	if _, err := os.Stat(kubePath); !os.IsNotExist(err) {
		t.Errorf("staged file survived the job: stat err = %v", err)
	}
}

// TestSandboxDeniesAnUngrantedDevShmFile is the control for the test above: a
// /dev/shm file that was never granted through ReadFiles is denied under real
// nono exactly as any other ungranted path would be — proving the read above
// succeeded because of the grant, not because /dev/shm is somehow exempt from
// the sandbox's default-deny.
func TestSandboxDeniesAnUngrantedDevShmFile(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	requireNono(t)

	data := sandboxDataDir(t)
	work := t.TempDir()

	granted := filepath.Join(executor.TmpfsRoot, "exocomp-secret-test-granted")
	ungranted := filepath.Join(executor.TmpfsRoot, "exocomp-secret-test-ungranted")
	for _, p := range []string{granted, ungranted} {
		if err := os.WriteFile(p, []byte("control-value"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		t.Cleanup(func(p string) func() { return func() { _ = os.Remove(p) } }(p))
	}

	profile := executor.ProfileFor(executor.KindCLI, executor.SandboxOptions{
		DataDir:    data,
		WritePaths: []string{work},
		ReadFiles:  []string{granted},
	})

	// CONTROL: the granted file reads fine, so a denial below is the sandbox's
	// doing and not some other misconfiguration.
	argv, err := profile.Command([]string{"cat", granted})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runIn := func(argv []string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = work // nono refuses CWD access without a grant on it.
		return cmd.CombinedOutput()
	}
	if out, err := runIn(argv); err != nil {
		t.Fatalf("granted /dev/shm file was denied, so the control is broken: %v\n%s", err, out)
	}

	// TEST: the ungranted file, sitting in the very same directory, is denied.
	argv, err = profile.Command([]string{"cat", ungranted})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	out, err := runIn(argv)
	if err == nil {
		t.Fatalf("an ungranted /dev/shm file was read under the sandbox:\n%s", out)
	}
	if !strings.Contains(string(out), "Permission denied") {
		t.Errorf("the read failed for a reason other than the sandbox: %v\n%s", err, out)
	}
}

// TestSandboxReadsAnIdentityFileOutsideTheCheckoutOnceGranted is the G5
// acceptance proof under real nono: an ssh identity key stored OUTSIDE the
// source checkout is unreadable to a job unless the per-job read grant names
// it, and readable the moment the grant does — the same file, the same
// sandbox, differing only in the declared grant. The key lives beside the work
// dir (the source checkout stand-in) and is granted through ReadFiles, exactly
// how the ansible driver folds Spec.SSHIdentityFiles.
func TestSandboxReadsAnIdentityFileOutsideTheCheckoutOnceGranted(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	requireNono(t)

	data := sandboxDataDir(t)
	work := t.TempDir() // the source checkout a job reads from and runs in
	keys := t.TempDir() // the key's real home, outside the checkout

	identity := filepath.Join(keys, "id_ed25519")
	if err := os.WriteFile(identity, []byte("not-a-real-key\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	// DENIED-WITHOUT: the same profile shape the ansible driver builds, minus
	// the identity grant, refuses the read — "Load key … Permission denied" is
	// exactly this denial at the ssh layer.
	denied := executor.ProfileFor(executor.KindAnsible, executor.SandboxOptions{
		DataDir:    data,
		WritePaths: []string{work},
	})
	argv, err := denied.Command([]string{"cat", identity})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runIn := func(argv []string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = work
		return cmd.CombinedOutput()
	}
	out, err := runIn(argv)
	if err == nil {
		t.Fatalf("the key read without the grant:\n%s", out)
	}
	if !strings.Contains(string(out), "Permission denied") {
		t.Errorf("denial came from somewhere other than the sandbox: %v\n%s", err, out)
	}

	// ALLOWED-WITH: the grant names the file, and the very same read succeeds.
	granted := executor.ProfileFor(executor.KindAnsible, executor.SandboxOptions{
		DataDir:    data,
		WritePaths: []string{work},
		ReadFiles:  []string{identity},
	})
	argv, err = granted.Command([]string{"cat", identity})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	out, err = runIn(argv)
	if err != nil {
		t.Fatalf("the granted identity read was denied: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "not-a-real-key") {
		t.Errorf("granted read did not return the key bytes: %q", out)
	}
}
