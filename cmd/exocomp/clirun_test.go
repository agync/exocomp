package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
)

// runCLIRoot drives a verb through a real root command so the global
// --assume-yes/--output/--idempotency-key persistent flags exist, exactly as a
// shell invocation has them. stdin is a buffer, i.e. non-interactive.
func runCLIRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// runCLIRootStdin is runCLIRoot for a verb that reads its value from stdin.
func runCLIRootStdin(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// registerFakeTool writes an executable stand-in for a vendor CLI and
// allow-lists it, which is the only way a `run-cli` job can exist at all.
func registerFakeTool(t *testing.T, name, body string, extra ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	args := append([]string{"cli-tool", "add", name, "--binary", bin, "--assume-yes"}, extra...)
	if out, err := runCLIRoot(t, args...); err != nil {
		t.Fatalf("cli-tool add: %v (%s)", err, out)
	}
	return bin
}

// cliRunCmd is a bare command for driving cliRun directly, which is how a
// non-zero job outcome is observed: the verb calls os.Exit on one.
func cliRunCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := &cobra.Command{}
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SetContext(context.Background())
	return c
}

// TestRunCLIRefusesABinaryNotOnTheAllowList is the core safety property: the
// refusal happens BEFORE anything is enqueued, so an unregistered binary never
// reaches the queue, a worker, or the job table.
func TestRunCLIRefusesABinaryNotOnTheAllowList(t *testing.T) {
	initForSource(t)

	out, err := runCLIRoot(t, "run-cli", "curl", "--", "https://example.invalid")
	if err == nil {
		t.Fatalf("an unregistered binary ran: %s", out)
	}
	if !strings.Contains(out, "allow-list") {
		t.Errorf("refusal does not say why: %q", out)
	}

	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("the refusal still wrote %d job row(s): %+v", len(jobs), jobs)
	}
}

// TestRunCLIExitCodeDecidesTheJob runs an allow-listed binary end to end
// through the queue and pins both directions of the exit-code doctrine.
func TestRunCLIExitCodeDecidesTheJob(t *testing.T) {
	initForSource(t)
	registerFakeTool(t, "vendorctl", "echo \"ran $*\"\nexit ${EXIT_AS:-0}\n")

	out, err := runCLIRoot(t, "run-cli", "vendorctl", "--", "deploy", "site")
	if err != nil {
		t.Fatalf("run-cli: %v (%s)", err, out)
	}
	if !strings.Contains(out, "ran deploy site") {
		t.Errorf("the tool's output did not reach the job's events: %q", out)
	}
	if !strings.Contains(out, "succeeded") {
		t.Errorf("job did not settle succeeded: %q", out)
	}

	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected one job row, got %d", len(jobs))
	}
	if jobs[0].Kind != "cli" {
		t.Errorf("job kind = %q, want cli", jobs[0].Kind)
	}
	if jobs[0].State != store.StateSucceeded {
		t.Errorf("job state = %s, want succeeded", jobs[0].State)
	}
	if jobs[0].ExitCode == nil || *jobs[0].ExitCode != 0 {
		t.Errorf("job exit code = %v, want 0", jobs[0].ExitCode)
	}
}

// TestRunCLIFailingToolFailsTheJob is the other direction: a non-zero exit is
// the verdict, and it reaches the job row.
func TestRunCLIFailingToolFailsTheJob(t *testing.T) {
	initForSource(t)
	registerFakeTool(t, "failctl", "echo 'vendor api rejected the request' >&2\nexit 9\n")

	// The verb calls os.Exit on a non-zero job, so the outcome is read from the
	// job row rather than from the process: run the inner path directly.
	code, err := cliRun(cliRunCmd(t), cliRunSpec{tool: "failctl"})
	if err != nil {
		t.Fatalf("run-cli: %v", err)
	}
	if code != 9 {
		t.Fatalf("exit code = %d, want the tool's own 9", code)
	}

	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != store.StateFailed {
		t.Fatalf("job = %+v, want one failed job", jobs)
	}
	if jobs[0].ExitCode == nil || *jobs[0].ExitCode != 9 {
		t.Errorf("job exit code = %v, want 9", jobs[0].ExitCode)
	}
}

// TestRunCLIArgvLandsAsLiteralArgvElements proves at the CLI seam what the
// driver test proves at the executor seam: a metacharacter-laden argument is
// one argv element, never interpolated, because nothing on this path is a
// shell.
func TestRunCLIArgvLandsAsLiteralArgvElements(t *testing.T) {
	initForSource(t)
	registerFakeTool(t, "echoctl", "echo \"count=$#\"\nfor a in \"$@\"; do echo \"arg:$a\"; done\n")

	nasty := `; touch /tmp/pwned && $(id) | wc -l > /dev/null *`
	out, err := runCLIRoot(t, "run-cli", "echoctl", "--", "apply", nasty)
	if err != nil {
		t.Fatalf("run-cli: %v (%s)", err, out)
	}
	if !strings.Contains(out, "count=2") {
		t.Fatalf("the tool did not see exactly 2 arguments; it was split:\n%s", out)
	}
	if !strings.Contains(out, "arg:"+nasty) {
		t.Fatalf("the argument did not arrive verbatim:\n%s", out)
	}

	// The job row records the same argv the process was given.
	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	argv := jobs[0].Args.Argv
	if len(argv) != 3 || argv[0] != "echoctl" || argv[2] != nasty {
		t.Fatalf("recorded argv = %q, want [echoctl apply <literal>]", argv)
	}
}

// TestRunCLIRequireApprovalGatesTheJob: --require-approval must land the job in
// awaiting_approval, where no worker can claim it, with a pending approval to
// decide.
func TestRunCLIRequireApprovalGatesTheJob(t *testing.T) {
	initForSource(t)
	registerFakeTool(t, "vendorctl", "exit 0\n")

	out, err := runCLIRoot(t, "run-cli", "vendorctl", "--require-approval", "--", "destroy", "prod")
	if err != nil {
		t.Fatalf("run-cli --require-approval: %v (%s)", err, out)
	}
	if !strings.Contains(out, "awaiting_approval") {
		t.Errorf("gated run did not say so: %q", out)
	}

	st := openTestStore(t)
	ctx := context.Background()
	jobs, err := st.ListJobs(ctx, store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != store.StateAwaitingApproval {
		t.Fatalf("jobs = %+v, want one awaiting_approval job", jobs)
	}
	pending, err := st.ListApprovals(ctx, true)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(pending) != 1 || pending[0].JobID != jobs[0].ID {
		t.Fatalf("approvals = %+v, want one pending approval for job %d", pending, jobs[0].ID)
	}
}

// TestRunCLIRunSecretsInjectAsEnvNeverArgv seeds a sentinel through the
// credential plane end to end: the tool sees it in its environment, and it
// appears in neither the recorded argv nor the job's events.
func TestRunCLIRunSecretsInjectAsEnvNeverArgv(t *testing.T) {
	initForSource(t)
	const sentinel = "exocomp-cli-sentinel-11f4e2-not-a-real-token"
	registerFakeTool(t, "vendorctl", `printf 'token len %s\n' "${#VENDOR_TOKEN}"`+"\n")

	if out, err := runCLIRootStdin(t, sentinel+"\n", "secrets", "add", "vendor-token"); err != nil {
		t.Fatalf("secrets add: %v (%s)", err, out)
	}
	if out, err := runCLIRoot(t, "run-secret", "add", "vendor", "VENDOR_TOKEN", "vendor-token"); err != nil {
		t.Fatalf("run-secret add: %v (%s)", err, out)
	}

	out, err := runCLIRoot(t, "run-cli", "vendorctl", "--run-secrets", "vendor", "--", "deploy")
	if err != nil {
		t.Fatalf("run-cli: %v (%s)", err, out)
	}
	if !strings.Contains(out, "token len "+strconv.Itoa(len(sentinel))) {
		t.Fatalf("the bound secret did not reach the tool's environment:\n%s", out)
	}
	if strings.Contains(out, sentinel) {
		t.Fatal("the run secret's value appeared in the run's output")
	}

	st := openTestStore(t)
	ctx := context.Background()
	jobs, err := st.ListJobs(ctx, store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, a := range jobs[0].Args.Argv {
		if strings.Contains(a, sentinel) {
			t.Fatalf("the run secret's value was recorded on the job argv: %q", a)
		}
	}
	// The binding NAME travels on the row; the value never does.
	if jobs[0].Args.RunSecretBinding != "vendor" {
		t.Errorf("run-secret binding = %q, want vendor", jobs[0].Args.RunSecretBinding)
	}
	events, err := st.ListJobEvents(ctx, jobs[0].ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range events {
		if strings.Contains(e.Msg, sentinel) || strings.Contains(e.RawJSON, sentinel) {
			t.Fatalf("the run secret's value appeared in a job event: %q", e.Msg)
		}
	}
}

// TestRunCLIHonoursTheRegistration: the fixed arguments are prepended to every
// invocation, and an argument outside the registered pattern is refused before
// anything is enqueued.
func TestRunCLIHonoursTheRegistration(t *testing.T) {
	initForSource(t)
	registerFakeTool(t, "kubectl", "echo \"argv=$*\"\n",
		"--fixed-arg", "--context=homelab", "--arg-pattern", `[a-z0-9./=-]+`)

	out, err := runCLIRoot(t, "run-cli", "kubectl", "--", "get", "pods")
	if err != nil {
		t.Fatalf("run-cli: %v (%s)", err, out)
	}
	if !strings.Contains(out, "argv=--context=homelab get pods") {
		t.Errorf("the registration's fixed arguments were not applied: %q", out)
	}

	out, err = runCLIRoot(t, "run-cli", "kubectl", "--", "delete; rm -rf /")
	if err == nil {
		t.Fatalf("an argument outside the registered pattern ran: %s", out)
	}
	if !strings.Contains(out, "pattern") {
		t.Errorf("refusal does not name the pattern: %q", out)
	}

	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("the pattern refusal wrote a job row: %d rows", len(jobs))
	}
}

// TestRunCLIPrivilegedMarksTheRow proves the --privileged flag threads onto the
// job spec and lands on the row, and that the queue records the unsandboxed
// execution on the ledger. The tool still runs (a privileged job at the disabled
// tier is the same plain path an ordinary run takes there), so the assertion is
// the persisted marker and the audit event, not the tier probe.
func TestRunCLIPrivilegedMarksTheRow(t *testing.T) {
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
	if !jobs[0].Args.Privileged {
		t.Error("the --privileged flag did not reach the job row")
	}
	if events := jobEventText(t, jobs[0].ID); !strings.Contains(events, "UNSANDBOXED") {
		t.Errorf("no unsandboxed audit event on the ledger:\n%s", events)
	}
}

// TestRunCLIIdempotencyKeyDedupsToOneJob: a retried run with the same key must
// resolve to the job the first call created, not a second one.
func TestRunCLIIdempotencyKeyDedupsToOneJob(t *testing.T) {
	initForSource(t)
	registerFakeTool(t, "vendorctl", "exit 0\n")

	for range 2 {
		if out, err := runCLIRoot(t, "run-cli", "vendorctl",
			"--idempotency-key", "deploy-2026-08-23-a", "--", "deploy"); err != nil {
			t.Fatalf("run-cli: %v (%s)", err, out)
		}
	}

	st := openTestStore(t)
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("the idempotency key created %d jobs, want 1", len(jobs))
	}
}

// TestRunCLIOutputJSONEmitsTheJobEnvelope: the JSON path prints the wire
// envelope and nothing else, so a harness can parse stdout whole.
func TestRunCLIOutputJSONEmitsTheJobEnvelope(t *testing.T) {
	initForSource(t)
	registerFakeTool(t, "vendorctl", "echo noise\nexit 0\n")

	out, err := runCLIRoot(t, "run-cli", "vendorctl", "--output", "json", "--", "deploy")
	if err != nil {
		t.Fatalf("run-cli --output json: %v (%s)", err, out)
	}
	var envelope struct {
		Kind string `json:"kind"`
		Data struct {
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("stdout is not one JSON envelope (%v):\n%s", err, out)
	}
	if envelope.Kind != "job.get" {
		t.Errorf("envelope kind = %q, want job.get", envelope.Kind)
	}
	if envelope.Data.State != "succeeded" {
		t.Errorf("envelope state = %q, want succeeded", envelope.Data.State)
	}
}

// TestCLIToolAddTakesTheConfirmContract: allow-listing a binary widens what
// this control plane can execute, so it is gated like any destructive verb —
// and refuses on a non-interactive session rather than hanging on stdin.
func TestCLIToolAddTakesTheConfirmContract(t *testing.T) {
	initForSource(t)

	out, err := runCLIRoot(t, "cli-tool", "add", "kubectl", "--binary", "/usr/bin/kubectl")
	if err == nil {
		t.Fatalf("allow-listing without confirmation succeeded: %s", out)
	}
	if !strings.Contains(out, "--assume-yes") {
		t.Errorf("refusal did not name the flag to pass: %q", out)
	}

	st := openTestStore(t)
	rows, err := st.ListCLITools(context.Background())
	if err != nil {
		t.Fatalf("list cli tools: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("the unconfirmed add still registered %d tool(s)", len(rows))
	}

	if out, err := runCLIRoot(t, "cli-tool", "add", "kubectl",
		"--binary", "/usr/bin/kubectl", "--assume-yes"); err != nil {
		t.Fatalf("--assume-yes did not proceed: %v (%s)", err, out)
	}
	if out, err := runCLIRoot(t, "cli-tool", "list"); err != nil || !strings.Contains(out, "kubectl") {
		t.Fatalf("list = %q (%v)", out, err)
	}
	if out, err := runCLIRoot(t, "cli-tool", "rm", "kubectl"); err != nil {
		t.Fatalf("cli-tool rm: %v (%s)", err, out)
	}
}
