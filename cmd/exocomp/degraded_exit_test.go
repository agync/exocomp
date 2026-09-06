package main

import (
	"io"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestJobsListAcceptsDegradedState: `jobs list --state degraded` is a valid
// filter — a degraded run is a real, queryable terminal state, so the CLI must
// not refuse the word. Unknown states are still refused.
func TestJobsListAcceptsDegradedState(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	out, _, err := runRoot(t, []string{"jobs", "list", "--state", "degraded"})
	if err != nil {
		t.Fatalf("jobs list --state degraded: %v", err)
	}
	if !strings.Contains(out, "no jobs") {
		t.Fatalf("empty-store list = %q, want the no-jobs line", out)
	}

	_, _, err = runRoot(t, []string{"jobs", "list", "--state", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "not a job state") {
		t.Fatalf("--state bogus err = %v, want a not-a-job-state refusal", err)
	}
}

// TestPlaybookReportJobOutcomeDegradedExitsZero pins B4 on the local path: a
// degraded run is a tolerated success, so the shell code a playbook run reports
// is 0 however non-zero the underlying exit was — a tolerated offline host must
// not fail the caller or whatever watches the code. A real failure keeps its
// exit code.
func TestPlaybookReportJobOutcomeDegradedExitsZero(t *testing.T) {
	t.Parallel()

	var code = 4
	degraded := store.Job{ID: 7, Runbook: "site.yml", State: store.StateDegraded, ExitCode: &code}
	if rc := reportJobOutcome(io.Discard, degraded); rc != 0 {
		t.Errorf("reportJobOutcome(degraded) = %d, want 0 (a tolerated run is not a caller failure)", rc)
	}

	failed := store.Job{ID: 8, Runbook: "site.yml", State: store.StateFailed, ExitCode: &code}
	if rc := reportJobOutcome(io.Discard, failed); rc != 4 {
		t.Errorf("reportJobOutcome(failed) = %d, want the real 4 preserved", rc)
	}
}

// TestRemoteJobExitDegradedIsZero pins B4 on the remote path: a followed
// degraded job exits 0, the same mapping reportJobOutcome applies locally.
func TestRemoteJobExitDegradedIsZero(t *testing.T) {
	t.Parallel()

	var code = 4
	view := wire.JobView{ID: 7, State: "degraded", ExitCode: &code}
	if rc := remoteJobExit(view); rc != 0 {
		t.Errorf("remoteJobExit(degraded) = %d, want 0", rc)
	}
}

// TestRemotePlaybookRunDegradedExitsZero drives the whole remote follow path
// against a stub that settles the job degraded: the command must exit 0 and
// print the degraded state, not treat the tolerated run as a failure.
func TestRemotePlaybookRunDegradedExitsZero(t *testing.T) {
	stub := &writeAPIStub{jobState: "degraded"}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{
		"--remote", "box", "playbook", "run", "site.yml", "--source", "demo",
	})
	if err != nil {
		t.Fatalf("remote playbook run (degraded): %v", err)
	}
	if !strings.Contains(out, "job 9  state: degraded") {
		t.Fatalf("output missing the degraded settle line:\n%s", out)
	}
}
