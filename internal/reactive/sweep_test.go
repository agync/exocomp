package reactive

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// captureHandler collects every logged message, so a test can assert what did
// and did not reach the engine logger.
type captureHandler struct {
	msgs []string
}

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.msgs = append(c.msgs, r.Message)
	return nil
}
func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }

// TestSweepStepToleratesItemErrors pins the shared sweep error policy in its
// pure form: a per-item failure is logged and the sweep continues, while a
// cancelled context stops the sweep and logs nothing — the stop is the
// shutdown itself, not an item worth reporting.
func TestSweepStepToleratesItemErrors(t *testing.T) {
	capture := &captureHandler{}
	e := &Engine{log: slog.New(capture)}
	ctx := context.Background()

	if n, keep := e.sweepStep(ctx, "sweep x", "item 1", func() (int, error) {
		return 3, nil
	}); n != 3 || !keep {
		t.Fatalf("ok item = (%d, %v), want (3, true)", n, keep)
	}

	if n, keep := e.sweepStep(ctx, "sweep x", "item 2", func() (int, error) {
		return 0, errors.New("boom")
	}); n != 0 || !keep {
		t.Fatalf("failing item = (%d, %v), want (0, true) tolerated", n, keep)
	}
	if len(capture.msgs) != 1 || capture.msgs[0] != "reactive: sweep x" {
		t.Fatalf("logged %d messages %v, want the one failing item logged", len(capture.msgs), capture.msgs)
	}

	// A failed item during shutdown stops the sweep: the partial yield is
	// dropped (the sweep is over either way) and the log stays quiet, because
	// the shutdown — not the item — is what stopped it.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if n, keep := e.sweepStep(cancelCtx, "sweep x", "item 3", func() (int, error) {
		return 1, errors.New("boom")
	}); n != 0 || keep {
		t.Fatalf("failing item at shutdown = (%d, %v), want (0, false)", n, keep)
	}
	if len(capture.msgs) != 1 {
		t.Fatalf("shutdown failure logged %d messages, want the suppression to hold", len(capture.msgs))
	}

	// A cancelled context before the item runs never calls it.
	called := false
	if n, keep := e.sweepStep(cancelCtx, "sweep x", "item 4", func() (int, error) {
		called = true
		return 0, nil
	}); n != 0 || keep {
		t.Fatalf("cancelled-before item = (%d, %v), want (0, false)", n, keep)
	}
	if called {
		t.Fatal("item ran on an already-cancelled context")
	}
}

// TestSweepCheckToleratesABadTarget pins the per-target tolerance in the check
// sweep: a target whose probe cannot even start is logged and skipped, and the
// check keeps probing its siblings — the good target still gets its state row
// seeded. The seed absorbs the observation, so nothing is emitted.
func TestSweepCheckToleratesABadTarget(t *testing.T) {
	orig := probeHTTPClient
	defer func() { probeHTTPClient = orig }()
	// The shipped dial guard refuses loopback, so an unguarded client lets the
	// good target's httptest server answer up (the guard is pinned separately
	// in checks_internal_test.go).
	probeHTTPClient = func() *http.Client { return &http.Client{Timeout: probeTimeout} }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := labelsEngine(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// The check_row must exist for the state row's FK; the bad URL cannot be
	// stored (store validation refuses it), so the row is created with the good
	// target only and the probe set is built with both.
	created, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name:      "web",
		Kind:      store.CheckHTTP,
		IntervalS: 30,
		Targets:   []store.CheckTarget{{Name: "good", URL: srv.URL}},
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	c := store.Check{
		ID:   created.ID,
		Name: created.Name,
		Kind: created.Kind,
		Targets: []store.CheckTarget{
			{Name: "bad", URL: "://not-a-url"},
			{Name: "good", URL: srv.URL},
		},
	}

	raised, err := e.sweepCheck(ctx, c, now)
	if err != nil {
		t.Fatalf("sweepCheck with a bad target = %v, want nil (tolerated)", err)
	}
	if raised != 0 {
		t.Fatalf("sweepCheck raised %d events, want 0 (seed-only)", raised)
	}

	st, err := e.store.GetCheckState(ctx, c.ID, "good")
	if err != nil {
		t.Fatalf("good target was never probed: %v", err)
	}
	if st.Status != store.CheckUp {
		t.Fatalf("good target seeded %q, want %q", st.Status, store.CheckUp)
	}
}

// TestSweepCheckProbeErrorDoesNotAdvanceStreak pins the anti-regression the
// hysteresis feature exists for: a target one observation short of a declared
// transition whose probe ERRORS — an http target whose URL cannot even be
// parsed into a request — must not advance the streak, must not emit, and must
// not abort the sweep.
func TestSweepCheckProbeErrorDoesNotAdvanceStreak(t *testing.T) {
	capture := &captureHandler{}
	e := labelsEngine(t)
	e.log = slog.New(capture)
	ctx := context.Background()
	now := time.Now().UTC()

	// The bad URL cannot be stored (store validation refuses it), so the check
	// row is created with a probe-able target and the probe set is built with
	// the errored one.
	created, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name:      "web",
		Kind:      store.CheckHTTP,
		IntervalS: 30,
		Targets:   []store.CheckTarget{{Name: "flake", URL: "http://127.0.0.1:1/"}},
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	c := store.Check{
		ID:      created.ID,
		Name:    created.Name,
		Kind:    created.Kind,
		Targets: []store.CheckTarget{{Name: "flake", URL: "://not-a-url"}},
	}

	// Seed the streak one observation short of a declared transition, with the
	// emitted flag pinned to the same status so the only way an event is
	// emitted is for the streak to advance.
	if err := e.store.SetCheckState(ctx, store.CheckState{
		CheckID:     c.ID,
		TargetName:  "flake",
		Status:      store.CheckUp,
		Consecutive: DefaultHysteresis - 1,
		Emitted:     store.CheckUp,
		FirstSeen:   now,
		LastChange:  now,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	raised, err := e.sweepCheck(ctx, c, now)
	if err != nil {
		t.Fatalf("sweepCheck with an errored probe = %v, want nil (tolerated)", err)
	}
	if raised != 0 {
		t.Fatalf("sweepCheck raised %d events, want 0 (an errored probe must not emit)", raised)
	}
	if len(capture.msgs) != 1 || capture.msgs[0] != "reactive: sweep checks: probe" {
		t.Fatalf("logged %v, want the single probe-error warning", capture.msgs)
	}

	st, err := e.store.GetCheckState(ctx, c.ID, "flake")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.Consecutive != DefaultHysteresis-1 {
		t.Fatalf("streak advanced to %d, want %d (an errored probe must not advance the streak)",
			st.Consecutive, DefaultHysteresis-1)
	}
	if st.Emitted != store.CheckUp {
		t.Fatalf("emitted flag moved to %q, want %q", st.Emitted, store.CheckUp)
	}
}
