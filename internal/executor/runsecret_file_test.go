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

// tmpfsDir returns a fresh directory on a tmpfs mount, or skips: the file-
// staging path only writes onto RAM-only storage, so its success cases need a
// real tmpfs. /dev/shm is tmpfs on a normal Linux host; where it is not (a
// stripped container, a non-Linux runner) the test says so and skips rather than
// asserting against durable disk.
func tmpfsDir(t *testing.T) string {
	t.Helper()
	const base = "/dev/shm"
	ok, err := executor.RunDirTmpfs(base)
	if err != nil || !ok {
		t.Skipf("no tmpfs at %s (tmpfs=%v, err=%v)", base, ok, err)
	}
	dir, err := os.MkdirTemp(base, "exocomp-p3-")
	if err != nil {
		t.Fatalf("mkdir tmpfs dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// durableDir returns a fresh directory on the durable filesystem the checkout
// lives on, or skips when even that is tmpfs (some dev boxes mount the whole
// tree in RAM). It is the honest stand-in for a run dir the deployment forgot to
// tmpfs-back: the refusal path must trigger on exactly this.
func durableDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir, err := os.MkdirTemp(wd, "exocomp-p3-durable-")
	if err != nil {
		t.Fatalf("mkdir durable dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ok, err := executor.RunDirTmpfs(dir)
	if err != nil {
		t.Skipf("cannot probe %s: %v", dir, err)
	}
	if ok {
		t.Skipf("%s is tmpfs; the refusal path cannot be exercised here", dir)
	}
	return dir
}

const kubeconfigSentinel = "apiVersion: v1\nkind: Config\ntoken: s3cr3t-bearer\n"

// TestStageRunSecretFilesWritesTmpfsFile proves the helper materializes a named
// run-secret to a 0600 file on tmpfs, rewrites that env to the file's path, and
// leaves the other run-secrets as env values.
func TestStageRunSecretFilesWritesTmpfsFile(t *testing.T) {
	t.Parallel()
	dir := tmpfsDir(t)

	rs := executor.RunSecrets{
		"KUBECONFIG": kubeconfigSentinel,
		"KUBE_HOST":  "https://api.example:6443",
	}
	out, err := executor.StageRunSecretFiles(rs, []string{"KUBECONFIG"}, dir)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	path := out["KUBECONFIG"]
	if filepath.Dir(path) != dir {
		t.Fatalf("staged path %q is not under the run dir %q", path, dir)
	}
	if out["KUBE_HOST"] != "https://api.example:6443" {
		t.Errorf("an env-valued run-secret was rewritten: %q", out["KUBE_HOST"])
	}
	// The input map is untouched, so a caller can still fall back on it.
	if rs["KUBECONFIG"] != kubeconfigSentinel {
		t.Errorf("input map was mutated: %q", rs["KUBECONFIG"])
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("staged file mode = %o, want 600", info.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(body) != kubeconfigSentinel {
		t.Errorf("staged file contents = %q, want the sentinel", string(body))
	}
}

// TestStageRunSecretFilesRefusesNonTmpfs proves the fail-closed assertion: on a
// durable filesystem the helper stages NOTHING and returns a clear error, so a
// sealed bearer never lands on disk (ADR 0005, never a silent downgrade).
func TestStageRunSecretFilesRefusesNonTmpfs(t *testing.T) {
	t.Parallel()
	dir := durableDir(t)

	rs := executor.RunSecrets{"KUBECONFIG": kubeconfigSentinel}
	out, err := executor.StageRunSecretFiles(rs, []string{"KUBECONFIG"}, dir)
	if err == nil {
		t.Fatal("staging onto a non-tmpfs dir must be refused")
	}
	if !strings.Contains(err.Error(), "tmpfs") {
		t.Errorf("refusal error does not name the cause: %v", err)
	}
	if out != nil {
		t.Errorf("a refused staging returned a map: %v", out)
	}
	// Nothing was written: the directory the helper refused stays empty.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("refused staging left %d file(s) on durable disk", len(entries))
	}
}

// TestTofuFileStagingRefusesNonTmpfs proves the driver wires the fail-closed
// assertion: with RunSecretFiles set and the private root on durable disk, Run
// returns a tmpfs error, stages nothing, and never launches the tool.
func TestTofuFileStagingRefusesNonTmpfs(t *testing.T) {
	t.Parallel()
	privateRoot := durableDir(t)
	bin, logPath := envDumper(t, executor.TofuBinary)

	_, err := executor.NewTofu(executor.TofuOptions{
		Binary:         bin,
		Workspace:      "edge",
		BackendURL:     "http://127.0.0.1:41111/api/v1/tf/state/edge",
		BackendToken:   "tok",
		PrivateDirRoot: privateRoot,
		RunSecrets:     executor.RunSecrets{"KUBECONFIG": kubeconfigSentinel},
		RunSecretFiles: []string{"KUBECONFIG"},
	}).Run(context.Background(), executor.Spec{
		JobID: 1, Kind: executor.KindTofu, Argv: executor.TofuArgv("plan"), Dir: t.TempDir(),
	}, &collector{})
	if err == nil {
		t.Fatal("file staging onto a non-tmpfs private root must be refused")
	}
	if !strings.Contains(err.Error(), "tmpfs") {
		t.Errorf("refusal error does not name tmpfs: %v", err)
	}
	// The tool never ran: the refusal is before tofu init, so its env log — which
	// the script only creates on execution — does not exist.
	if _, statErr := os.Stat(logPath); statErr == nil {
		t.Error("the tool ran despite the staging refusal")
	}
}

// fileStagerBin writes a fake tofu that records its whole environment, its args,
// and — while the file still exists — the mode and body of $KUBECONFIG, so a
// test can prove the child saw a path whose file held the sentinel at 0600.
func fileStagerBin(t *testing.T, binName string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, binName)
	logPath = filepath.Join(dir, "run.log")
	script := "#!/bin/sh\n" +
		"env | sed 's/^/env:/' >> " + logPath + "\n" +
		"for a in \"$@\"; do printf 'arg:%s\\n' \"$a\" >> " + logPath + "; done\n" +
		"if [ -n \"$KUBECONFIG\" ] && [ -f \"$KUBECONFIG\" ]; then\n" +
		"  printf 'kubeconfig-mode:%s\\n' \"$(stat -c %a \"$KUBECONFIG\")\" >> " + logPath + "\n" +
		"  printf 'kubeconfig-body:%s\\n' \"$(cat \"$KUBECONFIG\" | tr '\\n' '~')\" >> " + logPath + "\n" +
		"fi\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write %s: %v", binName, err)
	}
	return bin, logPath
}

// TestTofuFileStagedSecretReachesChildAsPath is the end-to-end proof: a run-
// secret declared file-staged reaches tofu as KUBECONFIG=<path> whose 0600 file
// holds the sentinel, the sentinel value never appears in argv or an event, and
// the file is gone once the job ends (the private dir's RemoveAll ran).
func TestTofuFileStagedSecretReachesChildAsPath(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv("KUBECONFIG", "/leaked/from/parent")

	privateRoot := tmpfsDir(t)
	bin, logPath := fileStagerBin(t, executor.TofuBinary)

	var sink collector
	res, err := executor.NewTofu(executor.TofuOptions{
		Binary:         bin,
		Workspace:      "edge",
		BackendURL:     "http://127.0.0.1:41111/api/v1/tf/state/edge",
		BackendToken:   "tok",
		PrivateDirRoot: privateRoot,
		RunSecrets:     executor.RunSecrets{"KUBECONFIG": kubeconfigSentinel},
		RunSecretFiles: []string{"KUBECONFIG"},
	}).Run(context.Background(), executor.Spec{
		JobID:   1,
		Kind:    executor.KindTofu,
		Argv:    executor.TofuArgv("plan"),
		Dir:     t.TempDir(),
		Timeout: time.Minute,
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}

	lines := envLines(t, logPath)

	// The child saw KUBECONFIG as a path under the run's private tree, not the
	// value, and not the daemon's own inherited KUBECONFIG.
	var kubePath string
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "env:KUBECONFIG="); ok {
			kubePath = v
			break
		}
	}
	if kubePath == "" {
		t.Fatalf("KUBECONFIG did not reach the child: %v", lines)
	}
	if kubePath == kubeconfigSentinel {
		t.Fatal("KUBECONFIG carried the value, not a path")
	}
	if kubePath == "/leaked/from/parent" {
		t.Fatal("the daemon's own KUBECONFIG leaked into the run")
	}
	if !strings.HasPrefix(kubePath, privateRoot) {
		t.Errorf("staged path %q is not under the run's private root %q", kubePath, privateRoot)
	}
	if filepath.Base(kubePath) != "kubeconfig" {
		t.Errorf("staged file basename = %q, want kubeconfig", filepath.Base(kubePath))
	}

	// While it ran, the child read a 0600 file whose body was the sentinel.
	if !slices.Contains(lines, "kubeconfig-mode:600") {
		t.Errorf("staged file was not 0600: %v", lines)
	}
	wantBody := "kubeconfig-body:" + strings.ReplaceAll(kubeconfigSentinel, "\n", "~")
	if !slices.Contains(lines, wantBody) {
		t.Errorf("staged file body was not the sentinel: %v", lines)
	}

	// The value is absent from argv and from every job event.
	for _, l := range lines {
		if strings.HasPrefix(l, "arg:") && strings.Contains(l, "s3cr3t-bearer") {
			t.Errorf("the sentinel value reached argv: %q", l)
		}
	}
	for _, e := range sink.events {
		if strings.Contains(e.Msg, "s3cr3t-bearer") || strings.Contains(e.RawJSON, "s3cr3t-bearer") {
			t.Errorf("the sentinel value reached a job event: %+v", e)
		}
	}

	// The job is over: the private dir was removed, so the staged file is gone.
	if _, err := os.Stat(kubePath); !os.IsNotExist(err) {
		t.Errorf("staged file survived the job: stat err = %v", err)
	}
}
