package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestHelperLeavesAJobRunning is not a test: it is the body of the child
// process abandonJob kills. It claims a job and blocks, exactly as a container
// killed mid-run does.
func TestHelperLeavesAJobRunning(t *testing.T) {
	if os.Getenv("EXOCOMP_SWEEP_HELPER") != "1" {
		t.Skip("helper process, driven by abandonJob")
	}
	_ = runDevRun(context.Background(), io.Discard, devRunSpec{argv: []string{"/bin/sleep", "30"}})
}

// abandonJob leaves the data dir in the state a killed exocompd leaves it in:
// a job row still marked running, owned by a pid that no longer exists. It
// produces that by killing a real process rather than by rewriting the row,
// because the row is not the interesting part — the dead pid is.
func abandonJob(t *testing.T) int64 {
	t.Helper()

	child := exec.Command(os.Args[0], "-test.run=TestHelperLeavesAJobRunning")
	child.Env = append(os.Environ(),
		"EXOCOMP_SWEEP_HELPER=1",
		"XDG_DATA_HOME="+os.Getenv("XDG_DATA_HOME"))
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_, _ = child.Process.Wait()
	})

	id := waitForRunningJob(t)
	// SIGKILL the whole group: no defer runs, nothing transitions the row.
	if err := syscall.Kill(-child.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	if _, err := child.Process.Wait(); err != nil {
		t.Fatalf("reap helper: %v", err)
	}

	if got := jobState(t, id); got != store.StateRunning {
		t.Fatalf("the killed process left job %d as %s, want running", id, got)
	}
	return id
}

func waitForRunningJob(t *testing.T) int64 {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st, err := openStore()
		if err == nil {
			jobs, listErr := st.ListJobs(context.Background(),
				store.JobFilter{States: []store.JobState{store.StateRunning}})
			_ = st.Close()
			if listErr == nil && len(jobs) == 1 {
				return jobs[0].ID
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the helper never claimed a job")
	return 0
}

func jobState(t *testing.T, id int64) store.JobState {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	job, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %d: %v", id, err)
	}
	return job.State
}

func TestDevRunSweepsAbandonedJobs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	useDataDir(t, t.TempDir())
	id := abandonJob(t)

	var out bytes.Buffer
	if err := runDevRun(context.Background(), &out, devRunSpec{argv: []string{"/bin/true"}}); err != nil {
		t.Fatalf("devrun: %v", err)
	}
	if got := jobState(t, id); got != store.StateInterrupted {
		t.Errorf("abandoned job is %s after devrun, want interrupted", got)
	}
	if !strings.Contains(out.String(), "swept 1 job") {
		t.Errorf("devrun did not report the sweep: %q", out.String())
	}
}

func TestPlaybookSweepsAbandonedJobs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook not on PATH")
	}
	useDataDir(t, t.TempDir())
	id := abandonJob(t)

	play := filepath.Join(t.TempDir(), "noop.yml")
	body := "- hosts: localhost\n  connection: local\n  gather_facts: false\n" +
		"  tasks:\n    - ansible.builtin.debug: msg=ok\n"
	if err := os.WriteFile(play, []byte(body), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	var out, warn bytes.Buffer
	if _, err := runPlaybook(context.Background(), &out, &warn, playbookSpec{playbook: play}); err != nil {
		t.Fatalf("playbook run: %v\n%s%s", err, out.String(), warn.String())
	}
	if got := jobState(t, id); got != store.StateInterrupted {
		t.Errorf("abandoned job is %s after a playbook run, want interrupted", got)
	}
}

// packageSources returns every non-test Go file of this package, so a guard
// over "every job path" is derived from what the package holds rather than
// from a list a new file can be added without touching.
func packageSources(t *testing.T) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	sources := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sources[name] = string(body)
	}
	if len(sources) == 0 {
		t.Fatal("the guard found no sources to check")
	}
	return sources
}

// TestEveryJobPathOpensTheStoreThroughOpenJobStore guards the wiring rather
// than one instance of it. The sweep was called by exactly one command, which
// is how a job abandoned by a dead process survived every other entry point.
func TestEveryJobPathOpensTheStoreThroughOpenJobStore(t *testing.T) {
	sources := packageSources(t)

	var plumbing string
	for name, body := range sources {
		if strings.Contains(body, "func openJobStore(") {
			plumbing = name
		}
	}
	if plumbing == "" {
		t.Fatal("no file declares openJobStore")
	}

	for name, body := range sources {
		// Only the file that declares the sweeping opener may reach past it.
		if name != plumbing && strings.Contains(body, "store.Open(") {
			t.Errorf("%s opens the store directly; use openJobStore so the sweep runs", name)
		}
		if !strings.Contains(body, "queue.New(") {
			continue
		}
		// A file handed a *store.Store by its caller opens nothing itself.
		if !strings.Contains(body, "openStore(") && !strings.Contains(body, "store.Open(") {
			continue
		}
		// The invariant is that the sweep runs, not the name it runs under: serve
		// opens the store itself so it can acquire the cross-host lease BEFORE the
		// sweep (a fenced-out standby must not retire a live primary's rows), then
		// reconciles explicitly via ReconcileNonTerminal. Every other job path uses
		// the coupled opener.
		if !strings.Contains(body, "openJobStore(") && !strings.Contains(body, "ReconcileNonTerminal(") {
			t.Errorf("%s runs jobs over a store it opened without the sweep; use openJobStore", name)
		}
	}
}
