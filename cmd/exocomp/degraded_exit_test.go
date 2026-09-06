package main

import (
	"io"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

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
