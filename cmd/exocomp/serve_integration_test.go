package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"exocomp.dev/exocomp/internal/secrets"
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
	useDataDir(t, t.TempDir())
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

	// ... while a real `workspace apply` runs against the same daemon. The CLI
	// enqueues and follows; the daemon is the only process that claims a row.
	applyOut, applyCode := runCLIExit(t, bin, "workspace", "apply", "wsp")

	waitFor(t, "the injected workspace job to finish", 60*time.Second, func() bool {
		return jobState(t, injected).Terminal()
	})
	if got := jobState(t, injected); got != store.StateSucceeded {
		t.Errorf("workspace job the daemon ran is %s, want succeeded\n%s", got, d.dump())
	}
	if applyCode != 0 || !strings.Contains(applyOut, "state: succeeded") {
		t.Errorf("workspace apply exited %d:\n%s", applyCode, applyOut)
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

// TestServeRefusesASecondInstance drives two real processes against one data
// dir: the second must refuse rather than double-fire every schedule and race
// the first for every queued job. Once the holder is gone its lock is free —
// nothing has to clean up after it.
func TestServeRefusesASecondInstance(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)

	runCLI(t, bin, "init")

	first := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	first.waitForLine(t, &first.stdout, "scheduler started", 30*time.Second)

	out, err := runCLIOutput(bin, "serve", "--port", "0")
	if err == nil {
		t.Fatalf("a second serve started against the same data dir:\n%s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Errorf("second serve exited with %v, want exit status 1\n%s", err, out)
	}
	if pid := fmt.Sprintf("pid %d", first.cmd.Process.Pid); !strings.Contains(out, pid) {
		t.Errorf("second serve did not name the holder (%s):\n%s", pid, out)
	}

	first.signal(t, syscall.SIGTERM)
	first.waitExit(t, 60*time.Second)

	second := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	second.waitForLine(t, &second.stdout, "scheduler started", 30*time.Second)
	second.signal(t, syscall.SIGTERM)
	second.waitExit(t, 60*time.Second)
}

// TestDaemonRunsWhatTheCLIEnqueues is the daemon-aware CLI: with `serve` up,
// `workspace plan|apply` and `playbook run` enqueue, follow the job's events,
// and exit with the job's own exit code. Neither command starts a queue of its
// own, so nothing races the daemon for the row — the stand-in tool records the
// pid that ran it, and it is the daemon's.
func TestDaemonRunsWhatTheCLIEnqueues(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook", "ansible-vault")
	bin := buildExocomp(t)
	// The stand-in fails `apply` with a code no part of the CLI produces on
	// its own, so an exit code that reaches the shell came from the job.
	tofuLog := fakeTool(t, executor.TofuBinary, "case \"$1\" in apply) exit 3 ;; esac\nexit 0")

	runCLI(t, bin, "init")
	sealSecret(t, bin, "lab/canary", vaultCanary)
	playbook := vaultedPlaybook(t, "lab", vaultCanary)

	module := t.TempDir()
	writeFile(t, filepath.Join(module, "main.tf"), "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)
	daemonPID := fmt.Sprintf("ppid=%d", d.cmd.Process.Pid)

	planOut, planCode := runCLIExit(t, bin, "workspace", "plan", "wsp")
	assertDaemonRan(t, planOut, "workspace plan")
	if planCode != 0 || !strings.Contains(planOut, "state: succeeded") {
		t.Errorf("workspace plan exited %d, want 0 and a succeeded job:\n%s", planCode, planOut)
	}

	applyOut, applyCode := runCLIExit(t, bin, "workspace", "apply", "wsp")
	assertDaemonRan(t, applyOut, "workspace apply")
	if applyCode != 3 {
		t.Errorf("workspace apply exited %d, want the job's own 3:\n%s", applyCode, applyOut)
	}
	if !strings.Contains(applyOut, "state: failed") {
		t.Errorf("workspace apply did not report the failed job:\n%s", applyOut)
	}
	if invocations := readFile(t, tofuLog); !strings.Contains(invocations, daemonPID) {
		t.Errorf("tofu was not run by the daemon (%s):\n%s", daemonPID, invocations)
	}

	runOut, runCode := runCLIExit(t, bin, "playbook", "run", "--vault-project", "lab", playbook)
	assertDaemonRan(t, runOut, "playbook run")
	if runCode != 0 || !strings.Contains(runOut, "state: succeeded") {
		t.Errorf("playbook run exited %d, want 0 and a succeeded job:\n%s", runCode, runOut)
	}
	// The daemon derived the vault password (nothing else decrypts the play's
	// only variable) and the value it printed reached neither the event rows
	// nor this command's own output.
	events := jobEventText(t, lastJobID(t, "playbook"))
	if !strings.Contains(events, secrets.Marker) {
		t.Errorf("no redacted value in the job's events, so nothing was decrypted:\n%s", events)
	}
	assertRedacted(t, events+runOut, vaultCanary, vaultPassword(t, "lab"))

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestDaemonRunsBackupsAndVendorFromTheCLI is the rest of the daemon-aware
// CLI. A schedule saturates every worker, so both commands have to wait behind
// work they did not enqueue — which is the shape that used to make a drain
// claim a foreign row, fail it through the wrong driver, and never idle.
func TestDaemonRunsBackupsAndVendorFromTheCLI(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	fakeRestic(t, 0)
	fakeTool(t, "ansible-galaxy", "exit 0")

	runCLI(t, bin, "init")
	repo := filepath.Join(t.TempDir(), "repo")
	runCLI(t, bin, "backup", "add-target", "--name", "local", "--kind", "restic_local", "--uri", repo)
	// One fire a second, five seconds of work each: more than four workers can
	// keep up with, so the queue is saturated and never empty.
	runCLI(t, bin, "schedule", "add", "hog", "--cron", "@every 1s", "--kind", "shell",
		"--", "/bin/sleep", "5")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)
	waitFor(t, "the daemon's workers to fill up", 30*time.Second, func() bool {
		return len(jobsIn(t, store.StateRunning)) == serveWorkers &&
			len(jobsIn(t, store.StateQueued)) > 0
	})

	backupOut, backupCode := runCLIExit(t, bin, "backup", "run")
	assertDaemonRan(t, backupOut, "backup run")
	if backupCode != 0 || !strings.Contains(backupOut, "succeeded") {
		t.Errorf("backup run exited %d:\n%s\n%s", backupCode, backupOut, d.dump())
	}

	requirements := filepath.Join(t.TempDir(), "requirements.yml")
	writeFile(t, requirements, "collections: []\n")
	vendorOut, vendorCode := runCLIExit(t, bin, "playbook", "vendor", "--requirements", requirements)
	assertDaemonRan(t, vendorOut, "playbook vendor")
	if vendorCode != 0 || !strings.Contains(vendorOut, "state: succeeded") {
		t.Errorf("playbook vendor exited %d:\n%s\n%s", vendorCode, vendorOut, d.dump())
	}

	// Nothing the schedule queued was run by the wrong driver.
	for _, job := range jobsWithRunbook(t, "hog") {
		if events := jobEventText(t, job.ID); strings.Contains(events, "not a backup job") {
			t.Errorf("a CLI claimed schedule job %d and failed it:\n%s", job.ID, events)
		}
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// jobsWithRunbook returns every job a runbook has produced, in any state.
func jobsWithRunbook(t *testing.T, runbook string) []store.Job {
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
	var matching []store.Job
	for _, job := range jobs {
		if job.Runbook == runbook {
			matching = append(matching, job)
		}
	}
	return matching
}

// assertDaemonRan checks a command took the daemon path rather than draining a
// queue of its own.
func assertDaemonRan(t *testing.T, out, what string) {
	t.Helper()
	if !strings.Contains(out, "queued for the running daemon") {
		t.Errorf("%s did not hand its job to the daemon:\n%s", what, out)
	}
}

// vaultCanary is sealed as a secret row, vault-encrypted into a playbook, and
// printed by that playbook. One string has to survive decryption — which only
// the right derived password allows — and then be scrubbed out of every event.
const vaultCanary = "vault-canary-2f7b41"

// TestServeRunsAVaultedAnsibleJob proves the daemon rebuilds an ansible job's
// vault password from the project the row names. The job row carries the
// project and never the password; a daemon that could not derive it would fail
// the play at decryption time.
func TestServeRunsAVaultedAnsibleJob(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook", "ansible-vault")
	bin := buildExocomp(t)

	runCLI(t, bin, "init")
	sealSecret(t, bin, "lab/canary", vaultCanary)
	playbook := vaultedPlaybook(t, "lab", vaultCanary)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	job := enqueueJob(t, store.NewJob{
		Runbook: "playbook",
		Kind:    executor.KindAnsible,
		Trigger: "test",
		Args: store.JobArgs{
			Argv:         executor.AnsibleArgv(playbook, ""),
			VaultProject: "lab",
		},
	})
	waitFor(t, "the ansible job to finish", 120*time.Second, func() bool {
		return jobState(t, job).Terminal()
	})

	events := jobEventText(t, job)
	if got := jobState(t, job); got != store.StateSucceeded {
		t.Fatalf("the vaulted ansible job is %s, want succeeded\n%s\n%s", got, events, d.dump())
	}
	assertRedacted(t, events, vaultCanary, vaultPassword(t, "lab"))
	// The play printed the decrypted value, so the marker is the proof that
	// decryption happened at all — a failed decryption never gets this far.
	if !strings.Contains(events, secrets.Marker) {
		t.Errorf("no redacted value in the job's events, so nothing was decrypted:\n%s", events)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestServeShutdownLetsAnInFlightBackupFinish is the SIGTERM-mid-backup half
// of the gate. The backup leg writes its own backup_run row when it ends, so
// the daemon has to outlive it: a shutdown that cancelled the leg would leave
// the database claiming a run that nothing is running.
func TestServeShutdownLetsAnInFlightBackupFinish(t *testing.T) {
	useDataDir(t, t.TempDir())
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

// TestServeForcedCancelFinishesTheBackupRow is the same shutdown with no grace
// at all: the leg is cancelled mid-run, and the backup_run row it opened still
// has to reach a terminal state before the process is gone. Nothing else in
// the system moves that row — a boot sweep repairs jobs, not backup runs.
func TestServeForcedCancelFinishesTheBackupRow(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	fakeRestic(t, 10)

	runCLI(t, bin, "init")
	repo := filepath.Join(t.TempDir(), "repo")
	runCLI(t, bin, "backup", "add-target", "--name", "local", "--kind", "restic_local", "--uri", repo)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--drain-grace", "0")
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

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)

	if got := jobState(t, job); got != store.StateCancelled {
		t.Errorf("the cancelled backup job is %s, want cancelled\n%s", got, d.dump())
	}
	if runs := backupRunsInState(t, store.BackupRunning); len(runs) != 0 {
		t.Errorf("%d backup_run row(s) left running after the leg was cancelled", len(runs))
	}
	if runs := backupRunsInState(t, store.BackupCancelled); len(runs) != 1 {
		t.Errorf("%d cancelled backup_run row(s), want the one that was in flight\n%s",
			len(runs), d.dump())
	}
	assertNothingLeftRunning(t, d)
}

// TestServeShutdownRetiresWhatItCannotFinish covers the other end of the grace
// window: work that outlasts it is cancelled rather than waited on forever,
// and every row this daemon owned still reaches a terminal state before the
// process exits — there is nothing for the next boot's sweep to find.
func TestServeShutdownRetiresWhatItCannotFinish(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)

	runCLI(t, bin, "init")
	runCLI(t, bin, "schedule", "add", "slow", "--cron", "@every 1s", "--kind", "shell",
		"--", "/bin/sleep", "60")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--drain-grace", "200ms")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)
	// Generous: the daemon + its sleep jobs are competing for CPU, and a starved
	// host (e.g. a parallel -race suite) can take much longer than a fresh one to
	// get serveWorkers running and one more queued. The condition is what matters,
	// not the deadline.
	waitFor(t, "the queue to fill up", 90*time.Second, func() bool {
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

// runCLIExit runs one command and returns its output and the exit code the
// shell would see. A command that failed to run at all fails the test: that is
// not an exit code.
func runCLIExit(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	out, err := runCLIOutput(bin, args...)
	if err == nil {
		return out, 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("exocomp %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out, exitErr.ExitCode()
}

// fakeTool puts a stand-in for one pinned tool ahead of the real one on PATH
// and returns the file it records its invocations in, each with the pid that
// spawned it — which process ran a job is a fact these tests need. The
// daemon's wiring is what they are about; whether restic and tofu themselves
// work is proven elsewhere, at a cost in seconds these tests cannot pay.
func fakeTool(t *testing.T, name, body string) string {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, name+".log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@ ppid=$PPID\" >> %q\n%s\n", log, body)
	writeFile(t, filepath.Join(dir, name), script)
	if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatalf("chmod stand-in %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// fakeRestic is a restic that takes sleep seconds to back anything up, so a
// shutdown lands in the middle of a leg rather than between two of them. The
// sleep gets its own descriptors: killing this script must close the pipes the
// driver is scanning, and a grandchild holding them open would make a
// cancelled leg look like one that ignored the cancel.
func fakeRestic(t *testing.T, sleep int) string {
	t.Helper()
	return fakeTool(t, "restic", fmt.Sprintf(`case "$3" in
  cat) exit 1 ;;
  backup)
    /bin/sleep %d >/dev/null 2>&1
    echo '{"message_type":"summary","snapshot_id":"5eb0dd4c","data_added":4096}'
    ;;
esac
exit 0`, sleep))
}

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is required for this test and is not on PATH", name)
		}
	}
}

// sealSecret seals a value through the CLI, so the row the daemon reads for
// its redactor is the one an operator would have made.
func sealSecret(t *testing.T, bin, name, value string) {
	t.Helper()
	cmd := exec.Command(bin, "secrets", "add", name, "--kind", "generic")
	cmd.Stdin = strings.NewReader(value)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("secrets add %s: %v\n%s", name, err, stderr.String())
	}
}

// vaultPassword derives what `playbook run` and the daemon both derive for a
// vault project: the test computes it from the envelope rather than being told.
func vaultPassword(t *testing.T, project string) string {
	t.Helper()
	dir, err := dataDir()
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()
	return base64.StdEncoding.EncodeToString(env.Derive(vaultPurpose, project))
}

// vaultedPlaybook writes a playbook whose only variable is vault-encrypted
// under the project's derived password, and which prints that variable.
func vaultedPlaybook(t *testing.T, project, value string) string {
	t.Helper()

	dir := t.TempDir()
	pass := filepath.Join(dir, "vault-pass")
	writeFile(t, pass, vaultPassword(t, project)+"\n")

	encrypt := exec.Command("ansible-vault", "encrypt_string",
		"--vault-id", executor.DefaultVaultID+"@"+pass, "--name", "canary", value)
	encrypt.Stderr = io.Discard
	encrypted, err := encrypt.Output()
	if err != nil {
		t.Fatalf("ansible-vault encrypt_string: %v", err)
	}
	writeFile(t, filepath.Join(dir, "vars.yml"), string(encrypted)+"\n")

	playbook := filepath.Join(dir, "play.yml")
	writeFile(t, playbook, `- hosts: localhost
  connection: local
  gather_facts: false
  vars_files:
    - vars.yml
  tasks:
    - name: print the vaulted value
      ansible.builtin.debug:
        msg: "canary is {{ canary }}"
`)
	return playbook
}

// jobEventText joins everything a job wrote to its event rows.
func jobEventText(t *testing.T, jobID int64) string {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	events, err := st.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("list job events: %v", err)
	}
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Level + " " + e.Msg + " " + e.RawJSON + "\n")
	}
	return b.String()
}

// assertRedacted fails if any secret value survived into the events.
func assertRedacted(t *testing.T, events string, secretValues ...string) {
	t.Helper()
	for _, value := range secretValues {
		if strings.Contains(events, value) {
			t.Errorf("a secret value reached the job_event rows:\n%s", events)
		}
	}
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

// lastJobID returns the newest job with the given runbook.
func lastJobID(t *testing.T, runbook string) int64 {
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
	var id int64
	for _, job := range jobs {
		if job.Runbook == runbook && job.ID > id {
			id = job.ID
		}
	}
	if id == 0 {
		t.Fatalf("no job with runbook %q", runbook)
	}
	return id
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

// TestServeShutdownRetiresADeadProcessRow is the shutdown's other duty. A CLI
// killed while its row waited on a concurrency key this daemon held leaves a
// queued row owned by a pid that no longer exists: retiring only the rows this
// process owns leaves it queued until some later boot sweeps it.
func TestServeShutdownRetiresADeadProcessRow(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)

	runCLI(t, bin, "init")
	// One at a time, by key: whatever the CLI enqueues under the same key
	// cannot be claimed by anyone while the daemon's job runs.
	runCLI(t, bin, "schedule", "add", "hold", "--cron", "@every 1s", "--kind", "shell",
		"--concurrency-key", "held", "--", "/bin/sleep", "30")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--drain-grace", "200ms")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)
	waitFor(t, "the daemon to hold the key", 30*time.Second, func() bool {
		return len(jobsIn(t, store.StateRunning)) == 1
	})

	cli := exec.Command(bin, "devrun", "--concurrency-key", "held", "--", "/bin/true")
	if err := cli.Start(); err != nil {
		t.Fatalf("start devrun: %v", err)
	}
	var blocked int64
	waitFor(t, "the CLI's row to be queued behind the key", 30*time.Second, func() bool {
		for _, job := range jobsIn(t, store.StateQueued) {
			if job.Runbook == "devrun" {
				blocked = job.ID
				return true
			}
		}
		return false
	})
	if err := cli.Process.Kill(); err != nil {
		t.Fatalf("kill devrun: %v", err)
	}
	if _, err := cli.Process.Wait(); err != nil {
		t.Fatalf("reap devrun: %v", err)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)

	if got := jobState(t, blocked); got != store.StateInterrupted {
		t.Errorf("the killed CLI's row is %s after shutdown, want interrupted\n%s", got, d.dump())
	}
	assertNothingLeftRunning(t, d)
}
