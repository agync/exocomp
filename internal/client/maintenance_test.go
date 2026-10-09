package client

import (
	"context"
	"net/http"
	"testing"

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
