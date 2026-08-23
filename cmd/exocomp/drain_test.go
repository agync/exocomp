package main

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

// maintStub answers the daemon's maintenance endpoints. cordonInFlight is what a
// fresh cordon reports; readyInFlight is what a later readiness poll reports, so
// a test can model a daemon that drains between the cordon and the poll.
type maintStub struct {
	mu             sync.Mutex
	cordoned       bool
	cordonInFlight int
	readyInFlight  int
	interrupted    []wire.JobView
}

func (s *maintStub) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v wire.MaintenanceView) {
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindMaintenance, v)
	}
	mux.HandleFunc("POST /api/v1/maintenance/cordon", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.cordoned = true
		n := s.cordonInFlight
		s.mu.Unlock()
		write(w, wire.NewMaintenanceView(true, n))
	})
	mux.HandleFunc("POST /api/v1/maintenance/uncordon", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.cordoned = false
		s.mu.Unlock()
		write(w, wire.NewMaintenanceView(false, 0))
	})
	mux.HandleFunc("GET /api/v1/maintenance/readiness", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		c, n := s.cordoned, s.readyInFlight
		if s.readyInFlight > 0 {
			s.readyInFlight-- // model a job finishing between polls
		}
		s.mu.Unlock()
		write(w, wire.NewMaintenanceView(c, n))
	})
	mux.HandleFunc("GET /api/v1/maintenance/interrupted", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindJobList, s.interrupted)
	})
	return mux
}

// TestDrainCordonsAndReportsSafe: a bare `drain` cordons and, with nothing in
// flight, reports it is safe to reboot.
func TestDrainCordonsAndReportsSafe(t *testing.T) {
	stub := &maintStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "drain"})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !strings.Contains(out, "cordoned: yes") || !strings.Contains(out, "safe to reboot: yes") {
		t.Fatalf("output = %q", out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.cordoned {
		t.Fatal("drain did not cordon the daemon")
	}
}

// TestDrainJSONEnvelope: --output json re-emits the daemon's maintenance envelope
// byte for byte.
func TestDrainJSONEnvelope(t *testing.T) {
	stub := &maintStub{}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "--output", "json", "drain"})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	want, err := wire.MarshalSuccess(wire.KindMaintenance, wire.NewMaintenanceView(true, 0))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("json = %q, want %q", out, want)
	}
}

// TestDrainWaitUntilSafe: cordon reports a job in flight; --wait polls readiness
// until the daemon drains, then reports safe and exits zero.
func TestDrainWaitUntilSafe(t *testing.T) {
	stub := &maintStub{cordoned: true, cordonInFlight: 1, readyInFlight: 1}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "drain", "--wait"})
	if err != nil {
		t.Fatalf("drain --wait: %v", err)
	}
	if !strings.Contains(out, "waiting: 1 job(s) in flight") {
		t.Fatalf("expected a waiting line, got %q", out)
	}
	if !strings.Contains(out, "safe to reboot: yes") {
		t.Fatalf("did not report safe after draining: %q", out)
	}
}

// TestDrainResume lifts the cordon.
func TestDrainResume(t *testing.T) {
	stub := &maintStub{cordoned: true}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "drain", "--resume"})
	if err != nil {
		t.Fatalf("drain --resume: %v", err)
	}
	if !strings.Contains(out, "cordon lifted") {
		t.Fatalf("output = %q", out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.cordoned {
		t.Fatal("--resume did not uncordon the daemon")
	}
}

// TestDrainInterruptedRemote lists the interrupted jobs the daemon reports.
func TestDrainInterruptedRemote(t *testing.T) {
	stub := &maintStub{interrupted: []wire.JobView{
		{ID: 7, State: "interrupted", Runbook: "cut-off", Kind: "shell", QueuedAt: "2026-08-23T09:00:00Z"},
	}}
	pinnedRemote(t, stub.handler(t))

	out, _, err := runRoot(t, []string{"--remote", "box", "drain", "interrupted"})
	if err != nil {
		t.Fatalf("drain interrupted: %v", err)
	}
	if !strings.Contains(out, "interrupted") || !strings.Contains(out, "cut-off") {
		t.Fatalf("output = %q", out)
	}
}

// TestDrainNeedsARemote: cordon control has no local path, so a bare `drain`
// with no remote selected refuses rather than doing nothing.
func TestDrainNeedsARemote(t *testing.T) {
	withConfigHome(t)
	_, _, err := runRoot(t, []string{"drain"})
	if err == nil {
		t.Fatal("drain with no remote selected should fail")
	}
	if !strings.Contains(err.Error(), "--remote") {
		t.Fatalf("error = %v, want one naming --remote", err)
	}
}
