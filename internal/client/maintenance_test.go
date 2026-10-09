package client

import (
	"context"
	"net/http"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/wire"
)

// TestReloadDecodesEnvelope: the reload call POSTs the maintenance path and
// decodes the reload kind into the schedule count.
func TestReloadDecodesEnvelope(t *testing.T) {
	want := wire.NewReloadView(4)
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/maintenance/reload" {
			t.Errorf("path = %q, want the reload path", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindMaintenanceReload, want)
	}))

	c := newTestClient(t, srv, fp)
	got, err := c.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got.Schedules != 4 {
		t.Fatalf("reload schedules = %d, want 4 (%+v)", got.Schedules, got)
	}
}

// TestReloadRefusesUnexpectedKind: a wrong-kind answer is a client error, not a
// silent zero count.
func TestReloadRefusesUnexpectedKind(t *testing.T) {
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindMaintenance, wire.NewMaintenanceView(false, 0))
	}))

	c := newTestClient(t, srv, fp)
	if _, err := c.Reload(context.Background()); err == nil {
		t.Fatal("want an error for a wrong-kind reload answer")
	}
}

// TestRestartDecodesEnvelopeAndSendsForce: the restart call POSTs the restart
// path with the force query and decodes the pending view.
func TestRestartDecodesEnvelopeAndSendsForce(t *testing.T) {
	want := wire.NewRestartView(true, 3, time.Now().Add(time.Minute))
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/maintenance/restart" {
			t.Errorf("path = %q, want the restart path", r.URL.Path)
		}
		if got := r.URL.Query().Get("force"); got != "1" {
			t.Errorf("force query = %q, want 1", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindMaintenanceRestart, want)
	}))

	c := newTestClient(t, srv, fp)
	got, err := c.Restart(context.Background(), true)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !got.Pending || got.InFlight != 3 || got.Deadline != want.Deadline {
		t.Fatalf("restart view = %+v, want %+v", got, want)
	}
}

// TestRestartStatusDecodesEnvelope: the status read GETs the restart path and
// decodes the pending view.
func TestRestartStatusDecodesEnvelope(t *testing.T) {
	want := wire.NewRestartView(true, 1, time.Now().Add(time.Minute))
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/maintenance/restart" {
			t.Errorf("path = %q, want the restart path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindMaintenanceRestart, want)
	}))

	c := newTestClient(t, srv, fp)
	got, err := c.RestartStatus(context.Background())
	if err != nil {
		t.Fatalf("RestartStatus: %v", err)
	}
	if !got.Pending || got.InFlight != 1 || got.Deadline != want.Deadline {
		t.Fatalf("restart status = %+v, want %+v", got, want)
	}
}

// TestRestartRefusesUnexpectedKind: a wrong-kind answer is a client error.
func TestRestartRefusesUnexpectedKind(t *testing.T) {
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindMaintenance, wire.NewMaintenanceView(false, 0))
	}))

	c := newTestClient(t, srv, fp)
	if _, err := c.Restart(context.Background(), false); err == nil {
		t.Fatal("want an error for a wrong-kind restart answer")
	}
}
