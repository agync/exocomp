package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// fakeMaint is an in-memory stand-in for the daemon's drain controller: cordon
// flips a flag, and InFlight is set by the test to model running jobs.
type fakeMaint struct {
	mu       sync.Mutex
	cordoned bool
	inFlight int
}

func (f *fakeMaint) Cordon(context.Context) (MaintenanceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cordoned = true
	return MaintenanceState{Cordoned: f.cordoned, InFlight: f.inFlight}, nil
}

func (f *fakeMaint) Uncordon(context.Context) (MaintenanceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cordoned = false
	return MaintenanceState{Cordoned: f.cordoned, InFlight: f.inFlight}, nil
}

func (f *fakeMaint) State(context.Context) (MaintenanceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return MaintenanceState{Cordoned: f.cordoned, InFlight: f.inFlight}, nil
}

func (f *fakeMaint) setInFlight(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight = n
}

// serverWithMaint builds a web Server over the harness store with a maintenance
// seam attached, mirroring what `serve --web-addr` wires.
func serverWithMaint(t *testing.T, h *harness, m Maintenance) *Server {
	t.Helper()
	srv, err := New(Options{
		Store: h.store, Deriver: h.env, Plans: h.env, Maint: m,
		Version: "test", DataDir: h.dir,
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

func readView(t *testing.T, body string) wire.MaintenanceView {
	t.Helper()
	_, code, data := decodeEnvelope(t, body)
	if code != "" {
		t.Fatalf("wanted a success envelope, got error code %q (%s)", code, body)
	}
	var v wire.MaintenanceView
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode maintenance view: %v (%s)", err, body)
	}
	return v
}

// TestCordonIsOperatorScopedAndReflectsState is the gate: a readonly token cannot
// cordon, an operator token can, and readiness reflects what cordon set — flipping
// safe_to_reboot as the in-flight count drains.
func TestCordonIsOperatorScopedAndReflectsState(t *testing.T) {
	h := newHarness(t)
	maint := &fakeMaint{}
	srv := serverWithMaint(t, h, maint)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	readonly := h.mint(t, "ro", "readonly")
	operator := h.mint(t, "op", "operator")

	// A readonly token is refused before anything is cordoned.
	code, _, body := bearerPost(t, ts.URL, "/api/v1/maintenance/cordon", readonly, "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("readonly cordon status = %d, want 403 (%s)", code, body)
	}
	if _, ecode, _ := decodeEnvelope(t, body); ecode != wire.CodeForbidden {
		t.Fatalf("readonly cordon error code = %q, want forbidden", ecode)
	}
	if maint.cordoned {
		t.Fatal("a refused cordon still flipped the daemon state")
	}

	// One job still running: cordoned but NOT yet safe.
	maint.setInFlight(1)
	code, _, body = bearerPost(t, ts.URL, "/api/v1/maintenance/cordon", operator, "", nil)
	if code != http.StatusOK {
		t.Fatalf("operator cordon status = %d, want 200 (%s)", code, body)
	}
	v := readView(t, body)
	if !v.Cordoned || v.InFlight != 1 || v.SafeToReboot {
		t.Fatalf("after cordon with a job in flight: %+v, want cordoned, in_flight 1, not safe", v)
	}

	// The job drains: readiness (a read, so readonly may poll it) flips to safe.
	maint.setInFlight(0)
	code, _, body = bearerGet(t, ts.URL, "/api/v1/maintenance/readiness", readonly)
	if code != http.StatusOK {
		t.Fatalf("readiness status = %d, want 200 (%s)", code, body)
	}
	v = readView(t, body)
	if !v.Cordoned || v.InFlight != 0 || !v.SafeToReboot {
		t.Fatalf("after drain: %+v, want cordoned, in_flight 0, safe", v)
	}

	// Uncordon returns to the running state.
	code, _, body = bearerPost(t, ts.URL, "/api/v1/maintenance/uncordon", operator, "", nil)
	if code != http.StatusOK {
		t.Fatalf("uncordon status = %d, want 200 (%s)", code, body)
	}
	if v := readView(t, body); v.Cordoned {
		t.Fatalf("after uncordon: %+v, want not cordoned", v)
	}
}

// TestMaintenanceUnavailableWithoutSeam: a surface with no maintenance seam (a
// standalone web process) answers 503 rather than pretending to cordon a daemon
// it does not own.
func TestMaintenanceUnavailableWithoutSeam(t *testing.T) {
	h := newHarness(t)
	srv := serverWithMaint(t, h, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	operator := h.mint(t, "op", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/maintenance/cordon", operator, "", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("cordon without a seam status = %d, want 503 (%s)", code, body)
	}
}

// TestInterruptedListing surfaces the jobs retired to interrupted, so a post-boot
// operator can see what to re-drive.
func TestInterruptedListing(t *testing.T) {
	h := newHarness(t)
	srv := serverWithMaint(t, h, &fakeMaint{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx := context.Background()

	// CreateGatedJob lands the job in awaiting_approval, a state no worker
	// claims, so the harness's live queue runner cannot race the transition
	// below the way it would a job created straight into queued.
	job, err := h.store.CreateGatedJob(ctx, store.NewJob{
		Runbook: "cut-off", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create gated job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateAwaitingApproval, store.StateInterrupted); err != nil {
		t.Fatalf("interrupt job: %v", err)
	}

	readonly := h.mint(t, "ro", "readonly")
	code, _, body := bearerGet(t, ts.URL, "/api/v1/maintenance/interrupted", readonly)
	if code != http.StatusOK {
		t.Fatalf("interrupted status = %d, want 200 (%s)", code, body)
	}
	kind, ecode, data := decodeEnvelope(t, body)
	if ecode != "" {
		t.Fatalf("interrupted returned error %q", ecode)
	}
	if kind != wire.KindJobList {
		t.Fatalf("interrupted kind = %q, want %q", kind, wire.KindJobList)
	}
	var rows []wire.JobView
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("decode job list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != job.ID || rows[0].State != string(store.StateInterrupted) {
		t.Fatalf("interrupted rows = %+v, want the one interrupted job %d", rows, job.ID)
	}
}
