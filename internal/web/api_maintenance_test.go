package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// fakeMaint is an in-memory stand-in for the daemon's drain controller: cordon
// flips a flag, and InFlight is set by the test to model running jobs.
type fakeMaint struct {
	mu        sync.Mutex
	cordoned  bool
	inFlight  int
	schedules int
	reloaded  int

	restarted    int
	restartForce bool
	restartErr   error
	restartView  RestartState
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

func (f *fakeMaint) Reload(context.Context) (ReloadState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloaded++
	return ReloadState{Schedules: f.schedules}, nil
}

func (f *fakeMaint) Restart(_ context.Context, force bool) (RestartState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted++
	f.restartForce = force
	if f.restartErr != nil {
		return RestartState{}, f.restartErr
	}
	f.restartView = RestartState{
		Pending:  true,
		InFlight: f.inFlight,
		Deadline: time.Now().Add(time.Minute),
	}
	return f.restartView, nil
}

func (f *fakeMaint) RestartStatus(context.Context) (RestartState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restartView, nil
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

// TestReloadIsOperatorScopedAndCallsSeam is the gate: a readonly token cannot
// reload, an operator token can, and the answer carries the schedule count the
// seam reported.
func TestReloadIsOperatorScopedAndCallsSeam(t *testing.T) {
	h := newHarness(t)
	maint := &fakeMaint{schedules: 3}
	srv := serverWithMaint(t, h, maint)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	readonly := h.mint(t, "ro", "readonly")
	operator := h.mint(t, "op", "operator")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/maintenance/reload", readonly, "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("readonly reload status = %d, want 403 (%s)", code, body)
	}
	if _, ecode, _ := decodeEnvelope(t, body); ecode != wire.CodeForbidden {
		t.Fatalf("readonly reload error code = %q, want forbidden", ecode)
	}
	if maint.reloaded != 0 {
		t.Fatal("a refused reload still reached the daemon seam")
	}

	code, _, body = bearerPost(t, ts.URL, "/api/v1/maintenance/reload", operator, "", nil)
	if code != http.StatusOK {
		t.Fatalf("operator reload status = %d, want 200 (%s)", code, body)
	}
	kind, ecode, data := decodeEnvelope(t, body)
	if ecode != "" {
		t.Fatalf("reload returned error %q (%s)", ecode, body)
	}
	if kind != wire.KindMaintenanceReload {
		t.Fatalf("reload kind = %q, want %q", kind, wire.KindMaintenanceReload)
	}
	var v wire.ReloadView
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode reload view: %v (%s)", err, body)
	}
	if v.Schedules != 3 {
		t.Fatalf("reload schedules = %d, want 3", v.Schedules)
	}
	if maint.reloaded != 1 {
		t.Fatalf("reload reached the seam %d times, want 1", maint.reloaded)
	}
}

// TestReloadUnavailableWithoutSeam: a surface with no maintenance seam answers
// 503 rather than pretending to reload a daemon it does not own.
func TestReloadUnavailableWithoutSeam(t *testing.T) {
	h := newHarness(t)
	srv := serverWithMaint(t, h, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	operator := h.mint(t, "op", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/maintenance/reload", operator, "", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("reload without a seam status = %d, want 503 (%s)", code, body)
	}
}

// noSchedMaint models a daemon whose web surface started before the scheduler
// handle was wired: the seam refuses, and the handler answers 503.
type noSchedMaint struct {
	fakeMaint
}

func (m *noSchedMaint) Reload(context.Context) (ReloadState, error) {
	return ReloadState{}, ErrNoScheduler
}

// TestReloadWithoutSchedulerIsUnavailable: the nil-scheduler refusal surfaces
// as a 503, like the missing seam.
func TestReloadWithoutSchedulerIsUnavailable(t *testing.T) {
	h := newHarness(t)
	srv := serverWithMaint(t, h, &noSchedMaint{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	operator := h.mint(t, "op", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/maintenance/reload", operator, "", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("reload without a scheduler status = %d, want 503 (%s)", code, body)
	}
}

// readRestartView decodes a maintenance.restart success envelope.
func readRestartView(t *testing.T, body string) wire.RestartView {
	t.Helper()
	kind, code, data := decodeEnvelope(t, body)
	if code != "" {
		t.Fatalf("wanted a success envelope, got error code %q (%s)", code, body)
	}
	if kind != wire.KindMaintenanceRestart {
		t.Fatalf("restart kind = %q, want %q", kind, wire.KindMaintenanceRestart)
	}
	var v wire.RestartView
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode restart view: %v (%s)", err, body)
	}
	return v
}

// TestRestartIsOperatorScopedAndCallsSeam is the gate: a readonly token cannot
// restart, an operator token can, the answer carries the pending state, and the
// force query reaches the seam.
func TestRestartIsOperatorScopedAndCallsSeam(t *testing.T) {
	h := newHarness(t)
	maint := &fakeMaint{}
	srv := serverWithMaint(t, h, maint)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	readonly := h.mint(t, "ro", "readonly")
	operator := h.mint(t, "op", "operator")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/maintenance/restart", readonly, "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("readonly restart status = %d, want 403 (%s)", code, body)
	}
	if _, ecode, _ := decodeEnvelope(t, body); ecode != wire.CodeForbidden {
		t.Fatalf("readonly restart error code = %q, want forbidden", ecode)
	}
	if maint.restarted != 0 {
		t.Fatal("a refused restart still reached the daemon seam")
	}

	code, _, body = bearerPost(t, ts.URL, "/api/v1/maintenance/restart?force=1", operator, "", nil)
	if code != http.StatusOK {
		t.Fatalf("operator restart status = %d, want 200 (%s)", code, body)
	}
	v := readRestartView(t, body)
	if !v.Pending {
		t.Fatalf("restart answer = %+v, want pending", v)
	}
	if maint.restarted != 1 {
		t.Fatalf("restart reached the seam %d times, want 1", maint.restarted)
	}
	if !maint.restartForce {
		t.Fatal("the force query did not reach the seam")
	}
}

// TestRestartNoOpIsConflict: an unchanged binary is refused as a conflict, so a
// script branches on it rather than reading a restart that never happens.
func TestRestartNoOpIsConflict(t *testing.T) {
	h := newHarness(t)
	maint := &fakeMaint{restartErr: ErrNothingToRestart}
	srv := serverWithMaint(t, h, maint)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	operator := h.mint(t, "op", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/maintenance/restart", operator, "", nil)
	if code != http.StatusConflict {
		t.Fatalf("no-op restart status = %d, want 409 (%s)", code, body)
	}
	if _, ecode, _ := decodeEnvelope(t, body); ecode != wire.CodeConflict {
		t.Fatalf("no-op restart error code = %q, want conflict", ecode)
	}
}

// TestRestartStatusIsReadable: the status a `restart --wait` polls is a read, so
// a readonly token may watch it, and it reports the pending view.
func TestRestartStatusIsReadable(t *testing.T) {
	h := newHarness(t)
	maint := &fakeMaint{restartView: RestartState{Pending: true, InFlight: 2, Deadline: time.Now().Add(time.Minute)}}
	srv := serverWithMaint(t, h, maint)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	readonly := h.mint(t, "ro", "readonly")
	code, _, body := bearerGet(t, ts.URL, "/api/v1/maintenance/restart", readonly)
	if code != http.StatusOK {
		t.Fatalf("restart status = %d, want 200 (%s)", code, body)
	}
	v := readRestartView(t, body)
	if !v.Pending || v.InFlight != 2 || v.Deadline == "" {
		t.Fatalf("restart status = %+v, want pending with 2 in flight and a deadline", v)
	}
}

// TestRestartUnavailableWithoutSeam: a surface with no maintenance seam answers
// 503 rather than pretending to restart a daemon it does not own.
func TestRestartUnavailableWithoutSeam(t *testing.T) {
	h := newHarness(t)
	srv := serverWithMaint(t, h, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	operator := h.mint(t, "op", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/maintenance/restart", operator, "", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("restart without a seam status = %d, want 503 (%s)", code, body)
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
