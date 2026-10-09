package main

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/wire"
)

// restartStub answers the daemon's restart endpoints with a pending drain.
type restartStub struct {
	mu        sync.Mutex
	forced    bool
	restarted int
	deadline  time.Time
}

func (s *restartStub) view() wire.RestartView {
	return wire.NewRestartView(true, 0, s.deadline)
}

func (s *restartStub) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/maintenance/restart", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.restarted++
		s.forced = r.URL.Query().Get("force") == "1"
		v := s.view()
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindMaintenanceRestart, v)
	})
	mux.HandleFunc("GET /api/v1/maintenance/restart", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		v := s.view()
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindMaintenanceRestart, v)
	})
	return mux
}

// TestRestartReportsPending: a bare `restart` asks the daemon to restart and
// reports the pending drain, then returns without waiting.
func TestRestartReportsPending(t *testing.T) {
	stub := &restartStub{deadline: time.Now().Add(time.Minute)}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "restart"})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !strings.Contains(out, "restart requested") || !strings.Contains(out, "draining 0 job(s)") {
		t.Fatalf("output = %q, want the pending drain", out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.restarted != 1 {
		t.Fatalf("the daemon saw %d restart requests, want 1", stub.restarted)
	}
}

// TestRestartJSONEnvelope: --output json re-emits the daemon's restart envelope
// byte for byte.
func TestRestartJSONEnvelope(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	stub := &restartStub{deadline: deadline}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "--output", "json", "restart"})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	want, err := wire.MarshalSuccess(wire.KindMaintenanceRestart, wire.NewRestartView(true, 0, deadline))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("json = %q, want %q", out, want)
	}
}

// TestRestartForceSendsForce: --force reaches the daemon as the force query.
func TestRestartForceSendsForce(t *testing.T) {
	stub := &restartStub{deadline: time.Now().Add(time.Minute)}
	pinnedRemote(t, stub.handler(t))

	if _, _, err := runRoot(t, []string{"--remote", "box", "restart", "--force"}); err != nil {
		t.Fatalf("restart --force: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.forced {
		t.Fatal("--force did not reach the daemon")
	}
}

// TestRestartWaitUntilDaemonDown: --wait polls until the connection drops (the
// daemon re-execing) and reports it is restarting, exiting zero.
func TestRestartWaitUntilDaemonDown(t *testing.T) {
	pinnedRemote(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_ = wire.WriteSuccess(w, wire.KindMaintenanceRestart, wire.NewRestartView(true, 0, time.Now().Add(time.Minute)))
			return
		}
		// The daemon closed the listener for its re-exec.
		panic(http.ErrAbortHandler)
	}))

	out, _, err := runRoot(t, []string{"--remote", "box", "restart", "--wait"})
	if err != nil {
		t.Fatalf("restart --wait: %v", err)
	}
	if !strings.Contains(out, "daemon restarting") {
		t.Fatalf("output = %q, want the daemon-restarting line", out)
	}
}

// TestRestartWaitAborted: --wait reports a drain that aborted (the pending flag
// cleared while the daemon stayed up) as a failure, not a silent success.
func TestRestartWaitAborted(t *testing.T) {
	pinnedRemote(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_ = wire.WriteSuccess(w, wire.KindMaintenanceRestart, wire.NewRestartView(true, 1, time.Now().Add(time.Minute)))
			return
		}
		_ = wire.WriteSuccess(w, wire.KindMaintenanceRestart, wire.NewRestartView(false, 0, time.Time{}))
	}))

	_, _, err := runRoot(t, []string{"--remote", "box", "restart", "--wait"})
	if err == nil {
		t.Fatal("an aborted restart should exit nonzero")
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("error = %v, want one naming the abort", err)
	}
}

// TestRestartNeedsARemote: restart control has no local path, so a bare
// `restart` with no remote selected refuses rather than doing nothing.
func TestRestartNeedsARemote(t *testing.T) {
	withConfigHome(t)
	_, _, err := runRoot(t, []string{"restart"})
	if err == nil {
		t.Fatal("restart with no remote selected should fail")
	}
	if !strings.Contains(err.Error(), "--remote") {
		t.Fatalf("error = %v, want one naming --remote", err)
	}
}

// TestRestartForceHintsRedrive: a forced restart cancels the jobs still in
// flight, so the text output points at the recovery verb; JSON stays parseable.
func TestRestartForceHintsRedrive(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := maybeRedriveHint(cmd, outputText, true, 3); err != nil {
		t.Fatalf("maybeRedriveHint: %v", err)
	}
	if !strings.Contains(buf.String(), "exocomp jobs redrive") {
		t.Errorf("force hint missing the redrive pointer: %q", buf.String())
	}

	buf.Reset()
	if err := maybeRedriveHint(cmd, outputText, false, 3); err != nil {
		t.Fatalf("maybeRedriveHint: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a non-force restart printed a hint: %q", buf.String())
	}

	buf.Reset()
	if err := maybeRedriveHint(cmd, outputJSON, true, 3); err != nil {
		t.Fatalf("maybeRedriveHint: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("JSON mode printed a hint (stdout must stay parseable): %q", buf.String())
	}
}

// TestRestartWaitRefusedInsideJob is the self-deadlock gate: a job that runs
// `restart --wait` would wait on the daemon that is itself waiting for the job
// to exit, so the CLI refuses before contacting any daemon.
func TestRestartWaitRefusedInsideJob(t *testing.T) {
	withConfigHome(t)
	t.Setenv(jobContextEnv, "42")

	_, _, err := runRoot(t, []string{"restart", "--wait"})
	if err == nil || !strings.Contains(err.Error(), "inside a job") {
		t.Fatalf("restart --wait in a job = %v, want the inside-a-job refusal", err)
	}
	if !strings.Contains(err.Error(), "bare `restart`") {
		t.Fatalf("refusal = %v, want it to point at the bare form", err)
	}
}

// TestRestartBareAllowedInsideJob: the bare fire-and-forget form is the correct
// call from inside a job; the guard must not refuse it.
func TestRestartBareAllowedInsideJob(t *testing.T) {
	stub := &restartStub{deadline: time.Now().Add(time.Minute)}
	pinnedRemote(t, stub.handler(t))
	t.Setenv(jobContextEnv, "42")

	out, _, err := runRoot(t, []string{"--remote", "box", "restart"})
	if err != nil {
		t.Fatalf("bare restart in a job: %v", err)
	}
	if !strings.Contains(out, "restart requested") {
		t.Fatalf("output = %q, want the pending drain", out)
	}
}
