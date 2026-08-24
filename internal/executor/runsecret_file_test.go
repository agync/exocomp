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

const kubeconfigSentinel = "apiVersion: v1\nkind: Config\ntoken: s3cr3t-bearer\n"

// TestShmDirIsDevShm pins the literal value: every other test in this file
// asserts a staged path is "under executor.ShmDir", which would pass
// vacuously if ShmDir itself pointed somewhere durable. This is the one
// assertion against the literal string, so a change to the constant is
// caught here even though it would satisfy every relative check elsewhere.
func TestShmDirIsDevShm(t *testing.T) {
	t.Parallel()
	if executor.ShmDir != "/dev/shm" {
		t.Fatalf("executor.ShmDir = %q, want /dev/shm", executor.ShmDir)
	}
}

// TestStageRunSecretFilesWritesShmFile proves the helper materializes a named
// run-secret to a 0600 file under /dev/shm, rewrites that env to the file's
// path, and leaves the other run-secrets as env values.
func TestStageRunSecretFilesWritesShmFile(t *testing.T) {
	t.Parallel()

	rs := executor.RunSecrets{
		"KUBECONFIG": kubeconfigSentinel,
		"KUBE_HOST":  "https://api.example:6443",
	}
	out, staged, err := executor.StageRunSecretFiles(rs, []string{"KUBECONFIG"})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	t.Cleanup(func() {
		for i := range staged {
			_ = staged[i].Close()
		}
	})

	if len(staged) != 1 {
		t.Fatalf("staged = %d files, want 1", len(staged))
	}
	path := out["KUBECONFIG"]
	if staged[0].Path != path {
		t.Errorf("staged path %q does not match the rewritten env %q", staged[0].Path, path)
	}
	if filepath.Dir(path) != executor.ShmDir {
		t.Fatalf("staged path %q is not under %s", path, executor.ShmDir)
	}
	if !strings.HasPrefix(filepath.Base(path), "exocomp-secret-") {
		t.Errorf("staged basename %q does not carry the exocomp-secret- prefix", filepath.Base(path))
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

// TestStageRunSecretFilesNeverOnDurableDisk proves the bytes exist nowhere but
// /dev/shm (RAM): staging never touches the current working directory or any
// other path a caller might mistake for "the run's own tree".
func TestStageRunSecretFilesNeverOnDurableDisk(t *testing.T) {
	t.Parallel()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	rs := executor.RunSecrets{"KUBECONFIG": kubeconfigSentinel}
	_, staged, err := executor.StageRunSecretFiles(rs, []string{"KUBECONFIG"})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	t.Cleanup(func() {
		for i := range staged {
			_ = staged[i].Close()
		}
	})

	path := staged[0].Path
	if strings.HasPrefix(path, wd) {
		t.Fatalf("staged path %q landed under the working tree %q", path, wd)
	}
	if !strings.HasPrefix(path, executor.ShmDir+"/") {
		t.Fatalf("staged path %q is not under %s", path, executor.ShmDir)
	}
}

// TestStagedRunSecretFileCloseUnlinks proves Close removes the staged file:
// the secret is gone once the caller is done with it, the same lifecycle the
// job's own private tree had before this rework.
func TestStagedRunSecretFileCloseUnlinks(t *testing.T) {
	t.Parallel()
	rs := executor.RunSecrets{"KUBECONFIG": kubeconfigSentinel}
	_, staged, err := executor.StageRunSecretFiles(rs, []string{"KUBECONFIG"})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	path := staged[0].Path

	if err := staged[0].Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("staged file survived Close: stat err = %v", err)
	}
}

// TestStageRunSecretFilesEmptyFileEnvIsNoop proves an empty fileEnv list
// stages nothing and returns the input map unmodified — no allocation, no
// files, matching the empty-tolerance rule the ab:<workflow> resolution
// takes elsewhere.
func TestStageRunSecretFilesEmptyFileEnvIsNoop(t *testing.T) {
	t.Parallel()
	rs := executor.RunSecrets{"KUBECONFIG": kubeconfigSentinel}
	out, staged, err := executor.StageRunSecretFiles(rs, nil)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if staged != nil {
		t.Errorf("staged = %v, want nil", staged)
	}
	if out["KUBECONFIG"] != kubeconfigSentinel {
		t.Errorf("out = %v, want the input map unchanged", out)
	}
}

// TestSweepOrphanRunSecretFilesRemovesOrphans proves the startup sweep is the
// defense-in-depth backstop for a broken cleanup path: a file left behind
// under /dev/shm bearing the exocomp-secret- prefix — the shape a crash
// between staging and Close would leave — is removed, while an unrelated
// file with a different prefix (ansible's own POSIX semaphores, above all)
// is left untouched.
func TestSweepOrphanRunSecretFilesRemovesOrphans(t *testing.T) {
	orphan := filepath.Join(executor.ShmDir, "exocomp-secret-orphan-test")
	if err := os.WriteFile(orphan, []byte("leftover"), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(orphan) })

	unrelated := filepath.Join(executor.ShmDir, "not-exocomp-owned-test")
	if err := os.WriteFile(unrelated, []byte("leave me alone"), 0o600); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(unrelated) })

	swept, err := executor.SweepOrphanRunSecretFiles()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept < 1 {
		t.Errorf("swept = %d, want at least 1", swept)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan survived the sweep: stat err = %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("sweep removed a file outside its own prefix: %v", err)
	}
}

// fileStagerBin writes a fake tofu inside dir that records its whole
// environment, its args, and — while the file still exists — the mode and
// body of $KUBECONFIG, so a test can prove the child saw a path whose file
// held the sentinel at 0600. Both the binary and its log live in dir, so a
// caller running it under the sandbox can pass a directory the profile
// already grants read+write (the job's working directory), rather than the
// tool's own directory, which is granted read-only.
func fileStagerBin(t *testing.T, binName, dir string) (bin, logPath string) {
	t.Helper()
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
// secret declared file-staged reaches tofu as KUBECONFIG=<path> whose 0600
// file under /dev/shm holds the sentinel, the sentinel value never appears in
// argv or an event, and the file is gone once the job ends.
func TestTofuFileStagedSecretReachesChildAsPath(t *testing.T) {
	// t.Setenv forbids t.Parallel.
	t.Setenv("KUBECONFIG", "/leaked/from/parent")

	bin, logPath := fileStagerBin(t, executor.TofuBinary, t.TempDir())

	var sink collector
	res, err := executor.NewTofu(executor.TofuOptions{
		Binary:         bin,
		Workspace:      "edge",
		BackendURL:     "http://127.0.0.1:41111/api/v1/tf/state/edge",
		BackendToken:   "tok",
		PrivateDirRoot: t.TempDir(),
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

	// The child saw KUBECONFIG as a path under /dev/shm, not the value, and
	// not the daemon's own inherited KUBECONFIG.
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
	if !strings.HasPrefix(kubePath, executor.ShmDir+"/") {
		t.Errorf("staged path %q is not under %s", kubePath, executor.ShmDir)
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

	// The job is over: the staged file was unlinked when Run returned.
	if _, err := os.Stat(kubePath); !os.IsNotExist(err) {
		t.Errorf("staged file survived the job: stat err = %v", err)
	}
}
