package main

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
