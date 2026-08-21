package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// These tests drive a built `exocomp` binary as a process, because what they
// are about is what a process does: signals, exit codes, and the rows left in
// the database once it is gone. Nothing in here stands in for the daemon.

// TestServeIsTheDaemon covers the M2b serve gate in one run: the state backend
// answers, the boot sweep retires what a dead process left, a schedule keeps
// firing while a workspace job runs, SIGHUP rebuilds the schedules, and
// SIGTERM leaves nothing queued or running behind.
func TestServeIsTheDaemon(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	bin := buildExocomp(t)
	// The stand-in takes three seconds to apply, so the schedule below has a
	// workspace run to be concurrent with rather than a gap between two.
	tofuLog := fakeTool(t, executor.TofuBinary, "case \"$1\" in apply) /bin/sleep 3 ;; esac\nexit 0")

	runCLI(t, bin, "init")
	runCLI(t, bin, "schedule", "add", "tick", "--cron", "@every 1s", "--kind", "shell", "--", "/bin/true")

	module := t.TempDir()
	writeFile(t, filepath.Join(module, "main.tf"), "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)

	// A job a killed process left running: the boot sweep is what retires it.
	abandoned := abandonJob(t)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	line := d.waitForLine(t, &d.stdout, "state backend listening on http://", 30*time.Second)
	addr := strings.TrimPrefix(strings.TrimSpace(line), "state backend listening on http://")
	d.waitForLine(t, &d.stderr, "swept 1 job(s)", 10*time.Second)
	d.waitForLine(t, &d.stdout, "scheduler started", 10*time.Second)

	if got := jobState(t, abandoned); got != store.StateInterrupted {
		t.Errorf("abandoned job is %s after the boot sweep, want interrupted", got)
	}
	assertHealthz(t, addr)

	// A workspace job with no CLI attached to it: only the daemon can run it.
	firedBefore := countJobs(t, "tick")
	injected := enqueueJob(t, store.NewJob{
		Runbook: workspaceRunbookPrefix + "wsp",
		Kind:    executor.KindTofu,
		Trigger: "test",
		Args: store.JobArgs{
			Argv: executor.TofuArgv("apply", "-auto-approve"),
			Dir:  module,
		},
		ConcurrencyKey: "tofu:wsp",
	})

	// ... while a real `workspace apply` runs against the same daemon.
	applyOut, applyErr := runCLIOutput(bin, "workspace", "apply", "wsp")

	waitFor(t, "the injected workspace job to finish", 60*time.Second, func() bool {
		return jobState(t, injected).Terminal()
	})
	if got := jobState(t, injected); got != store.StateSucceeded {
		t.Errorf("workspace job the daemon ran is %s, want succeeded\n%s", got, d.dump())
	}
	// Both queues may claim a queued row, so the apply either ran in the CLI
	// or was already running in the daemon when the CLI looked.
	if applyErr != nil && !strings.Contains(applyOut, "state: running") {
		t.Errorf("workspace apply failed: %v\n%s", applyErr, applyOut)
	}
	if invocations := readFile(t, tofuLog); !strings.Contains(invocations, "init") ||
		!strings.Contains(invocations, "apply") {
		t.Errorf("tofu was not driven through init and apply:\n%s", invocations)
	}

	if fired := countJobs(t, "tick") - firedBefore; fired < 2 {
		t.Errorf("the @every 1s schedule fired %d job(s) while the workspace ran, want at least 2", fired)
	}

	// SIGHUP, and only SIGHUP: --reload-interval 0 leaves no other way for a
	// schedule added after startup to reach the running daemon.
	runCLI(t, bin, "schedule", "add", "tock", "--cron", "@every 1s", "--kind", "shell", "--", "/bin/true")
	time.Sleep(2 * time.Second)
	if n := countJobs(t, "tock"); n != 0 {
		t.Errorf("a schedule added after startup fired %d job(s) before any reload", n)
	}
	d.signal(t, syscall.SIGHUP)
	d.waitForLine(t, &d.stdout, "schedules reloaded", 10*time.Second)
	waitFor(t, "the reloaded schedule to fire", 15*time.Second, func() bool {
		return countJobs(t, "tock") > 0
	})

	runCLI(t, bin, "schedule", "disable", "tick")
	d.signal(t, syscall.SIGHUP)
	d.waitForLine(t, &d.stdout, "schedules reloaded", 10*time.Second)
	time.Sleep(200 * time.Millisecond) // let a fire that started before the reload land
	disabled := countJobs(t, "tick")
	time.Sleep(2 * time.Second)
	if after := countJobs(t, "tick"); after != disabled {
		t.Errorf("a disabled schedule fired %d more job(s) after the reload", after-disabled)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	if !strings.Contains(d.stdout.String(), "shutting down") {
		t.Errorf("serve did not report shutting down:\n%s", d.dump())
	}
	assertNothingLeftRunning(t, d)
}

// TestServeShutdownLetsAnInFlightBackupFinish is the SIGTERM-mid-backup half
// of the gate. The backup leg writes its own backup_run row when it ends, so
// the daemon has to outlive it: a shutdown that cancelled the leg would leave
// the database claiming a run that nothing is running.
func TestServeShutdownLetsAnInFlightBackupFinish(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	bin := buildExocomp(t)
	fakeRestic(t, 3)

	runCLI(t, bin, "init")
	repo := filepath.Join(t.TempDir(), "repo")
	runCLI(t, bin, "backup", "add-target", "--name", "local", "--kind", "restic_local", "--uri", repo)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	job := enqueueJob(t, store.NewJob{
		Runbook:        "backup",
		Kind:           backup.JobKind,
		Trigger:        "test",
		Args:           store.JobArgs{Argv: backup.Argv("local")},
		ConcurrencyKey: backup.ConcurrencyKey("local"),
	})
	waitFor(t, "the backup leg to start", 30*time.Second, func() bool {
		return len(backupRunsInState(t, store.BackupRunning)) == 1
	})

	signalled := time.Now()
	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)

	// The stand-in restic sleeps three seconds inside `restic backup`; an exit
	// faster than that is an exit that killed the leg.
	if waited := time.Since(signalled); waited < 1500*time.Millisecond {
		t.Errorf("serve exited %s after SIGTERM, so it cannot have waited for the leg", waited)
	}
	if got := jobState(t, job); got != store.StateSucceeded {
		t.Errorf("the in-flight backup job is %s, want succeeded\n%s", got, d.dump())
	}
	if runs := backupRunsInState(t, store.BackupRunning); len(runs) != 0 {
		t.Errorf("%d backup_run row(s) left running after shutdown", len(runs))
	}
	if runs := backupRunsInState(t, store.BackupSucceeded); len(runs) != 1 {
		t.Errorf("%d succeeded backup_run row(s), want the one that was in flight\n%s",
			len(runs), d.dump())
	}
	assertNothingLeftRunning(t, d)
}

// TestServeShutdownRetiresWhatItCannotFinish covers the other end of the grace
// window: work that outlasts it is cancelled rather than waited on forever,
// and every row this daemon owned still reaches a terminal state before the
// process exits — there is nothing for the next boot's sweep to find.
func TestServeShutdownRetiresWhatItCannotFinish(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	bin := buildExocomp(t)

	runCLI(t, bin, "init")
	runCLI(t, bin, "schedule", "add", "slow", "--cron", "@every 1s", "--kind", "shell",
		"--", "/bin/sleep", "60")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--drain-grace", "200ms")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)
	waitFor(t, "the queue to fill up", 30*time.Second, func() bool {
		return len(jobsIn(t, store.StateRunning)) == serveWorkers &&
			len(jobsIn(t, store.StateQueued)) > 0
	})

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)

	if !strings.Contains(d.stderr.String(), "unfinished after 200ms; cancelling") {
		t.Errorf("serve did not report cancelling the jobs that outlasted the grace:\n%s", d.dump())
	}
	if !strings.Contains(d.stdout.String(), "retired") {
		t.Errorf("serve did not report retiring the jobs it left queued:\n%s", d.dump())
	}
	assertNothingLeftRunning(t, d)
	if len(jobsIn(t, store.StateInterrupted)) == 0 {
		t.Error("no job was retired to interrupted")
	}
}

// assertNothingLeftRunning is the post-exit invariant every shutdown owes: a
// process that is gone leaves no row a boot sweep would have to repair.
func assertNothingLeftRunning(t *testing.T, d *daemon) {
	t.Helper()
	if left := jobsIn(t, store.StateQueued, store.StateRunning); len(left) != 0 {
		for _, job := range left {
			t.Errorf("job %d (%s) is still %s after serve exited", job.ID, job.Runbook, job.State)
		}
		t.Log(d.dump())
	}
}

func assertHealthz(t *testing.T, addr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatalf("build healthz request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		t.Fatalf("read healthz body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("healthz answered %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}
}

// daemon is a serve process under test.
type daemon struct {
	cmd            *exec.Cmd
	stdout, stderr syncBuffer
	done           chan error
	exited         bool
}

// startDaemon builds and starts the process, and makes sure it is gone by the
// end of the test whatever the test does.
func startDaemon(t *testing.T, bin string, args ...string) *daemon {
	t.Helper()

	d := &daemon{cmd: exec.Command(bin, args...), done: make(chan error, 1)}
	d.cmd.Stdout = &d.stdout
	d.cmd.Stderr = &d.stderr
	if err := d.cmd.Start(); err != nil {
		t.Fatalf("start %s %s: %v", bin, strings.Join(args, " "), err)
	}
	go func() { d.done <- d.cmd.Wait() }()

	t.Cleanup(func() {
		if d.exited {
			return
		}
		_ = d.cmd.Process.Kill()
		<-d.done
	})
	return d
}

func (d *daemon) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := d.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("send %s: %v", sig, err)
	}
}

// waitExit requires the process to exit successfully within timeout.
func (d *daemon) waitExit(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case err := <-d.done:
		d.exited = true
		if err != nil {
			t.Fatalf("serve exited with an error: %v\n%s", err, d.dump())
		}
	case <-time.After(timeout):
		t.Fatalf("serve did not exit within %s\n%s", timeout, d.dump())
	}
}

// waitForLine blocks until the stream contains a line starting with want, and
// returns that line.
func (d *daemon) waitForLine(t *testing.T, stream *syncBuffer, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, line := range strings.Split(stream.String(), "\n") {
			if strings.HasPrefix(line, want) {
				return line
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("serve never printed a line starting with %q\n%s", want, d.dump())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *daemon) dump() string {
	return "--- serve stdout ---\n" + d.stdout.String() + "--- serve stderr ---\n" + d.stderr.String()
}

// syncBuffer collects a child's output while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func buildExocomp(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is required to build the binary under test")
	}

	bin := filepath.Join(t.TempDir(), "exocomp")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build exocomp: %v\n%s", err, out)
	}
	return bin
}

// runCLI runs one command to completion and fails the test if it does not
// succeed. Its stdout is dropped on purpose: `init` prints the recovery kit,
// which must not reach a log (AGENTS.md invariant 4).
func runCLI(t *testing.T, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("exocomp %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
}

func runCLIOutput(bin string, args ...string) (string, error) {
	out, err := exec.Command(bin, args...).CombinedOutput()
	return string(out), err
}

// fakeTool puts a stand-in for one pinned tool ahead of the real one on PATH
// and returns the file it records its invocations in. The daemon's wiring is
// what these tests are about; whether restic and tofu themselves work is
// proven elsewhere, at a cost in seconds these tests cannot pay.
func fakeTool(t *testing.T, name, body string) string {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, name+".log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\n%s\n", log, body)
	writeFile(t, filepath.Join(dir, name), script)
	if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatalf("chmod stand-in %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// fakeRestic is a restic that takes sleep seconds to back anything up, so a
// shutdown lands in the middle of a leg rather than between two of them.
func fakeRestic(t *testing.T, sleep int) string {
	t.Helper()
	return fakeTool(t, "restic", fmt.Sprintf(`case "$3" in
  cat) exit 1 ;;
  backup)
    /bin/sleep %d
    echo '{"message_type":"summary","snapshot_id":"5eb0dd4c","data_added":4096}'
    ;;
esac
exit 0`, sleep))
}

func enqueueJob(t *testing.T, n store.NewJob) int64 {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	job, err := st.CreateJob(context.Background(), n)
	if err != nil {
		t.Fatalf("enqueue %s job: %v", n.Kind, err)
	}
	return job.ID
}

func jobsIn(t *testing.T, states ...store.JobState) []store.Job {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	jobs, err := st.ListJobs(context.Background(), store.JobFilter{States: states})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	return jobs
}

// countJobs counts every job a schedule has produced, whatever state it is in.
func countJobs(t *testing.T, runbook string) int {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	jobs, err := st.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	n := 0
	for _, job := range jobs {
		if job.Runbook == runbook {
			n++
		}
	}
	return n
}

func backupRunsInState(t *testing.T, state string) []store.BackupRun {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	targets, err := st.ListBackupTargets(ctx, true)
	if err != nil {
		t.Fatalf("list backup targets: %v", err)
	}

	var matching []store.BackupRun
	for _, target := range targets {
		runs, err := st.ListBackupRuns(ctx, target.ID, 0)
		if err != nil {
			t.Fatalf("list backup runs: %v", err)
		}
		for _, run := range runs {
			if run.State == state {
				matching = append(matching, run)
			}
		}
	}
	return matching
}

func waitFor(t *testing.T, what string, timeout time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if done() {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
