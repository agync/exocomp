package main

import (
	"net/http"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

// reloadStub answers the daemon's reload endpoint with a fixed schedule count.
func reloadStub(schedules int) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/maintenance/reload", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindMaintenanceReload, wire.NewReloadView(schedules))
	})
	return mux
}

// TestReloadReportsCount: a bare `reload` asks the daemon to rebuild its
// schedules and reports how many enabled schedules the store holds.
func TestReloadReportsCount(t *testing.T) {
	pinnedRemote(t, reloadStub(2))

	out, _, err := runRoot(t, []string{"--remote", "box", "reload"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !strings.Contains(out, "schedules reloaded (2 enabled in the store)") {
		t.Fatalf("output = %q, want the honest reloaded count", out)
	}
}

// TestReloadJSONEnvelope: --output json re-emits the daemon's reload envelope
// byte for byte.
func TestReloadJSONEnvelope(t *testing.T) {
	pinnedRemote(t, reloadStub(2))

	out, _, err := runRoot(t, []string{"--remote", "box", "--output", "json", "reload"})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	want, err := wire.MarshalSuccess(wire.KindMaintenanceReload, wire.NewReloadView(2))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Fatalf("json = %q, want %q", out, want)
	}
}

// TestReloadNeedsARemote: schedule control has no local path, so a bare
// `reload` with no remote selected refuses rather than doing nothing.
func TestReloadNeedsARemote(t *testing.T) {
	withConfigHome(t)
	_, _, err := runRoot(t, []string{"reload"})
	if err == nil {
		t.Fatal("reload with no remote selected should fail")
	}
	if !strings.Contains(err.Error(), "--remote") {
		t.Fatalf("error = %v, want one naming --remote", err)
	}
}
