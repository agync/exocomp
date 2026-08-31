package reactive

import (
	"context"
	"errors"
	"log/slog"
	"testing"
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
