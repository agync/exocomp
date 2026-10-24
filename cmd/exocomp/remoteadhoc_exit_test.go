package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// rootArgsEnv carries a command line to TestHelperRunRoot, which runs it in a
// child process: the remote ad-hoc path exits with the job's code through
// os.Exit, which a test can only observe from outside.
const rootArgsEnv = "EXOCOMP_TEST_ROOT_ARGS"

func TestHelperRunRoot(t *testing.T) {
	raw := os.Getenv(rootArgsEnv)
	if raw == "" {
		t.Skip("helper process only")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		os.Exit(99)
	}
	root := newRootCmd()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		os.Exit(exitCodeFor(err))
	}
	os.Exit(0)
}

// A remote ad-hoc run whose job fails exits with the job's code, in text and
// in JSON mode, as a local run does.
func TestRemoteCommandExitsWithTheJobsCode(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	d.mu.Lock()
	d.exitCode = 3
	d.mu.Unlock()

	for _, extra := range [][]string{nil, {"--output", "json"}} {
		args := append(append([]string{"--remote", "box"}, extra...), "command", "--host", "web", "--", "false")
		raw, _ := json.Marshal(args)
		child := exec.Command(os.Args[0], "-test.run=^TestHelperRunRoot$")
		child.Env = append(os.Environ(), rootArgsEnv+"="+string(raw))
		out, err := child.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Fatalf("%v: exit = %v, want 3\n%s", args, err, out)
		}
	}
}

// --become on the daemon's own host is an ordinary become: the run is enqueued
// with it, and no flag beyond --become is asked for.
func TestRemoteBecomeOnLoopbackIsEnqueued(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)

	if out, _, err := runRoot(t, []string{"--remote", "box", "command", "--become", "--host", "localhost", "--", "id"}); err != nil {
		t.Fatalf("command --become on localhost: %v\n%s", err, out)
	}
	job := onlyJob(t, d)
	if !slices.Contains(job.Args.Argv, "--become") || job.Args.Privileged {
		t.Fatalf("argv %v privileged %v, want an unprivileged run with --become", job.Args.Argv, job.Args.Privileged)
	}
}
