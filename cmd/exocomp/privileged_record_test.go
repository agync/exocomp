package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// attributedPrivileged is the enqueue-time record RecordPrivileged writes for a
// zero Actor (the local CLI operator). It is the record commit 8d4ab83 added to
// the CLI enqueue paths; the queue's own execution-time event ("running
// UNSANDBOXED...") says a run happened unsandboxed, but only this one attributes
// the escalation to whoever asked.
const attributedPrivileged = "run requested UNSANDBOXED (privileged) by local CLI operator"

// jobEvents joins a job's event messages so an assertion reads the whole ledger.
func jobEvents(t *testing.T, jobID int64) string {
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
		b.WriteString(e.Level + " " + e.Msg + "\n")
	}
	return b.String()
}

// TestRunCLIPrivilegedRecordsAttributedEvent covers the inline run-cli enqueue
// (cliRunInline, daemon not running): the job the CLI drains in-process carries
// the attributed enqueue-time record, not just the queue's execution-time one.
// Removing the RecordPrivileged call from cliRunInline makes this fail.
func TestRunCLIPrivilegedRecordsAttributedEvent(t *testing.T) {
	initForSource(t)
	registerFakeTool(t, "vendorctl", "exit 0\n")

	if _, err := cliRun(cliRunCmd(t), cliRunSpec{tool: "vendorctl", privileged: true}); err != nil {
		t.Fatalf("run-cli: %v", err)
	}

	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected one job row, got %d", len(jobs))
	}
	if !strings.Contains(jobEvents(t, jobs[0].ID), attributedPrivileged) {
		t.Fatalf("inline run-cli job carries no attributed privileged record:\n%s", jobEvents(t, jobs[0].ID))
	}
}

// TestRunCLIGatedPrivilegedRecordsAttributedEvent covers the gated run-cli path
// (gateCLIRun): the awaiting_approval job's ledger carries the attributed record
// even though no worker ever ran it. Removing the RecordPrivileged call from
// gateCLIRun makes this fail.
func TestRunCLIGatedPrivilegedRecordsAttributedEvent(t *testing.T) {
	initForSource(t)
	registerFakeTool(t, "vendorctl", "exit 0\n")

	out, err := runCLIRoot(t, "run-cli", "vendorctl", "--privileged", "--require-approval", "--", "destroy", "prod")
	if err != nil {
		t.Fatalf("run-cli --privileged --require-approval: %v (%s)", err, out)
	}
	if !strings.Contains(out, "awaiting_approval") {
		t.Errorf("gated run did not say so: %q", out)
	}

	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != store.StateAwaitingApproval {
		t.Fatalf("jobs = %+v, want one awaiting_approval job", jobs)
	}
	if !strings.Contains(jobEvents(t, jobs[0].ID), attributedPrivileged) {
		t.Fatalf("gated run-cli job carries no attributed privileged record:\n%s", jobEvents(t, jobs[0].ID))
	}
}

// TestAdhocPrivilegedRecordsAttributedEvent covers the inline ad-hoc enqueue
// (runAdhoc, daemon not running, no gate): the `command` job the process drains
// carries the attributed record. Removing the RecordPrivileged call from the
// inline branch of runAdhoc makes this fail.
func TestAdhocPrivilegedRecordsAttributedEvent(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible")

	var out, warn bytes.Buffer
	code, err := runAdhoc(context.Background(), &out, &warn, adhocSpec{
		runbook: "command", pattern: "localhost", module: "command",
		args: "echo privileged-adhoc", privileged: true,
	})
	if err != nil {
		t.Fatalf("runAdhoc: %v\n%s%s", err, out.String(), warn.String())
	}
	if code != 0 {
		t.Fatalf("command exited %d:\n%s", code, out.String())
	}
	if !strings.Contains(jobEvents(t, lastJobID(t, "command")), attributedPrivileged) {
		t.Fatalf("inline ad-hoc job carries no attributed privileged record:\n%s",
			jobEvents(t, lastJobID(t, "command")))
	}
}

// TestAdhocGatedPrivilegedRecordsAttributedEvent covers the gated ad-hoc path
// (gateAdhoc): the awaiting_approval job carries the attributed record. Removing
// the RecordPrivileged call from gateAdhoc makes this fail.
func TestAdhocGatedPrivilegedRecordsAttributedEvent(t *testing.T) {
	initForSource(t)

	var out, warn bytes.Buffer
	_, err := runAdhoc(context.Background(), &out, &warn, adhocSpec{
		runbook: "ansible", pattern: "localhost", module: "ansible.builtin.ping",
		requireApproval: true, privileged: true,
	})
	if err != nil {
		t.Fatalf("runAdhoc gated: %v\n%s%s", err, out.String(), warn.String())
	}
	if !strings.Contains(jobEvents(t, lastJobID(t, "ansible")), attributedPrivileged) {
		t.Fatalf("gated ad-hoc job carries no attributed privileged record:\n%s",
			jobEvents(t, lastJobID(t, "ansible")))
	}
}

// TestDaemonEnqueuePrivilegedRecordsAttributedEvent covers the daemon-sourced
// enqueue (enqueueAllForDaemon), the path a CLI uses when `exocomp serve` is up:
// the record is written at enqueue time, before the tail, so it is observable on
// the job even though the tail fails when no daemon is actually holding the
// serve lock. Removing the RecordPrivileged call from enqueueAllForDaemon makes
// this fail.
func TestDaemonEnqueuePrivilegedRecordsAttributedEvent(t *testing.T) {
	useDataDir(t, t.TempDir())
	dir, err := dataDir()
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	var out bytes.Buffer
	_, err = enqueueAllForDaemon(context.Background(), st, dir, []queue.Spec{{
		Runbook: "cli:vendorctl", Kind: executor.KindCLI, Trigger: "manual",
		Argv: executor.CLIArgv("vendorctl"), Privileged: true,
	}}, &out)
	if err == nil {
		t.Fatalf("expected the daemon-sourced enqueue to fail tailing without a running daemon")
	}

	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected one job row, got %d", len(jobs))
	}
	events, err := st.ListJobEvents(context.Background(), jobs[0].ID)
	if err != nil {
		t.Fatalf("list job events: %v", err)
	}
	var text strings.Builder
	for _, e := range events {
		text.WriteString(e.Msg + "\n")
	}
	if !strings.Contains(text.String(), attributedPrivileged) {
		t.Fatalf("daemon-sourced job carries no attributed privileged record:\n%s", text.String())
	}
}

// runnerFlagUsage finds the --runner flag's help text anywhere in a command tree,
// so a parent command (playbook) whose subcommand owns the flag is handled too.
func runnerFlagUsage(cmd *cobra.Command) string {
	if f := cmd.Flags().Lookup("runner"); f != nil {
		return f.Usage
	}
	for _, sub := range cmd.Commands() {
		if u := runnerFlagUsage(sub); u != "" {
			return u
		}
	}
	return ""
}

// TestRunnerFlagHelpStaysCapsOnly pins the honest --runner wording commit
// ddd634c introduced on every verb it touched, so the surface cannot silently
// regress to implying a runner image is built or run.
func TestRunnerFlagHelpStaysCapsOnly(t *testing.T) {
	const want = "(caps only; no runner image is built or run — that tier is deferred)"
	cases := []struct {
		name string
		cmd  *cobra.Command
	}{
		{"run-cli", newRunCLICmd()},
		{"playbook run", newPlaybookCmd()},
		{"run", newRunCmd()},
		{"workspace plan", newWorkspacePlanCmd()},
		{"workspace apply", newWorkspaceApplyCmd()},
		{"workspace destroy", newWorkspaceDestroyCmd()},
	}
	for _, c := range cases {
		usage := runnerFlagUsage(c.cmd)
		if usage == "" {
			t.Errorf("%s: no --runner flag found", c.name)
			continue
		}
		if !strings.Contains(usage, want) {
			t.Errorf("%s: --runner help = %q, want it to contain %q", c.name, usage, want)
		}
	}
}
