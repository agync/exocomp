package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// These tests close the gap the R1 lane deferred: the daemon's kindRouter did
// not serve executor.KindCLI, so a running `exocomp serve` both refused
// `run-cli` outright and left an approved gated cli job stuck with
// "sched: no driver for kind \"cli\"". They drive a built binary as a real
// process, the same way serve_integration_test.go does, because what is under
// test is which process actually runs the job.

// registerFakeCLITool writes an executable stand-in and allow-lists it against
// the running data dir through a real subprocess, exactly as an operator
// would with `exocomp cli-tool add`.
func registerFakeCLITool(t *testing.T, bin, name, body string, extra ...string) string {
	t.Helper()
	toolBin := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\n" + body
	writeFile(t, toolBin, script)
	if err := os.Chmod(toolBin, 0o755); err != nil {
		t.Fatalf("chmod fake cli tool %s: %v", name, err)
	}
	args := append([]string{"cli-tool", "add", name, "--binary", toolBin, "--assume-yes"}, extra...)
	runCLI(t, bin, args...)
	return toolBin
}

// soloPendingApproval returns the one pending approval a --require-approval
// run-cli just created.
func soloPendingApproval(t *testing.T) store.Approval {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	pending, err := st.ListApprovals(context.Background(), true)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending approvals = %d, want 1", len(pending))
	}
	return pending[0]
}

// TestDaemonRunsAnAllowListedCLITool is the first half of the R1 gap: with
// `exocomp serve` up, `run-cli` on an allow-listed tool must enqueue rather
// than refuse, and the daemon — not the CLI process — must run it to a real
// exit code.
func TestDaemonRunsAnAllowListedCLITool(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	runCLI(t, bin, "init")
	registerFakeCLITool(t, bin, "vendorctl", "echo \"ran $* ppid=$PPID\"\nexit 0\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)
	daemonPID := fmt.Sprintf("ppid=%d", d.cmd.Process.Pid)

	out, code := runCLIExit(t, bin, "run-cli", "vendorctl", "--", "deploy", "site")
	assertDaemonRan(t, out, "run-cli")
	if code != 0 {
		t.Fatalf("run-cli exited %d, want 0:\n%s\n%s", code, out, d.dump())
	}
	if !strings.Contains(out, "state: succeeded") {
		t.Errorf("run-cli did not report a succeeded job:\n%s", out)
	}
	if !strings.Contains(out, "ran deploy site "+daemonPID) {
		t.Errorf("the tool did not run as the daemon's own child (%s):\n%s", daemonPID, out)
	}

	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Kind != "cli" || jobs[0].State != store.StateSucceeded {
		t.Fatalf("jobs = %+v, want one succeeded cli job", jobs)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestDaemonRunsAnApprovedCLIJob is the exact failure the R1 lane flagged: a
// --require-approval cli job, once approved, used to fail on the daemon with
// `sched: no driver for kind "cli"` because the kindRouter fell through to the
// scheduler's shell/backup driver. It must now run to success instead.
func TestDaemonRunsAnApprovedCLIJob(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	runCLI(t, bin, "init")
	registerFakeCLITool(t, bin, "vendorctl", "echo ran\nexit 0\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "run-cli", "vendorctl", "--require-approval", "--", "deploy")
	if code != 0 || !strings.Contains(out, "awaiting_approval") {
		t.Fatalf("gated run-cli exited %d, want 0 and awaiting_approval:\n%s", code, out)
	}
	approval := soloPendingApproval(t)

	runCLI(t, bin, "approval", "decide", strconv.FormatInt(approval.ID, 10), "--approve")

	waitFor(t, "the approved cli job to finish", 30*time.Second, func() bool {
		return jobState(t, approval.JobID).Terminal()
	})
	events := jobEventText(t, approval.JobID)
	if got := jobState(t, approval.JobID); got != store.StateSucceeded {
		t.Fatalf("approved cli job is %s, want succeeded\n%s\n%s", got, events, d.dump())
	}
	if strings.Contains(events, `no driver for kind`) {
		t.Fatalf("the approved job hit the unrouted-kind failure the R1 lane flagged:\n%s", events)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestDaemonCLIJobFailsClosedWhenDeregistered proves the driver never falls
// back to a raw binary: an allow-list entry removed after a job was queued but
// before the daemon claims it must fail the job closed, with a clear message,
// not run whatever argv[0] happens to name.
func TestDaemonCLIJobFailsClosedWhenDeregistered(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	runCLI(t, bin, "init")
	registerFakeCLITool(t, bin, "vendorctl", "echo should-not-run\nexit 0\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	// Gate the run so the job sits in awaiting_approval — unclaimable — while
	// the allow-list entry is removed out from under it.
	out, code := runCLIExit(t, bin, "run-cli", "vendorctl", "--require-approval", "--", "deploy")
	if code != 0 || !strings.Contains(out, "awaiting_approval") {
		t.Fatalf("gated run-cli exited %d, want 0 and awaiting_approval:\n%s", code, out)
	}
	approval := soloPendingApproval(t)

	runCLI(t, bin, "cli-tool", "rm", "vendorctl")
	runCLI(t, bin, "approval", "decide", strconv.FormatInt(approval.ID, 10), "--approve")

	waitFor(t, "the deregistered cli job to fail", 30*time.Second, func() bool {
		return jobState(t, approval.JobID).Terminal()
	})
	events := jobEventText(t, approval.JobID)
	if got := jobState(t, approval.JobID); got != store.StateFailed {
		t.Fatalf("deregistered cli job is %s, want failed\n%s\n%s", got, events, d.dump())
	}
	if !strings.Contains(events, "no longer on the allow-list") {
		t.Errorf("failure does not say the tool was deregistered:\n%s", events)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestDaemonCLIRunSecretsInjectAsEnv proves the daemon path resolves run
// secrets the same way the no-daemon path does: a sentinel reaches the tool's
// environment and never the recorded argv or the job's own events.
func TestDaemonCLIRunSecretsInjectAsEnv(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	const sentinel = "exocomp-cli-router-sentinel-9c14aa-not-a-real-token"

	runCLI(t, bin, "init")
	sealSecret(t, bin, "vendor-token", sentinel)
	runCLI(t, bin, "run-secret", "add", "vendor", "VENDOR_TOKEN", "vendor-token")
	registerFakeCLITool(t, bin, "vendorctl", `printf 'token len %s\n' "${#VENDOR_TOKEN}"`+"\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "run-cli", "vendorctl", "--run-secrets", "vendor", "--", "deploy")
	assertDaemonRan(t, out, "run-cli")
	if code != 0 {
		t.Fatalf("run-cli exited %d, want 0:\n%s\n%s", code, out, d.dump())
	}
	if !strings.Contains(out, "token len "+strconv.Itoa(len(sentinel))) {
		t.Fatalf("the bound secret did not reach the tool's environment:\n%s", out)
	}
	if strings.Contains(out, sentinel) {
		t.Fatal("the run secret's value appeared in the run's own output")
	}

	events := jobEventText(t, lastJobID(t, "cli:vendorctl"))
	if strings.Contains(events, sentinel) {
		t.Fatalf("the run secret's value appeared in a job event:\n%s", events)
	}
	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, a := range jobs[0].Args.Argv {
		if strings.Contains(a, sentinel) {
			t.Fatalf("the run secret's value was recorded on the job argv: %q", a)
		}
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}
