package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/store"
)

// TestServeUntilSignalReturnsFatalWebSurfaceError reproduces finding #9:
// startWebSurface reports bind/TLS errors on a serveErr channel, but the
// daemon's main loop must select on it and treat a post-listening web-surface
// failure as fatal rather than printing "listening" and running on as if
// healthy.
func TestServeUntilSignalReturnsFatalWebSurfaceError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	s := sched.New(st, nil, nil)

	sigCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wantErr := errors.New("web surface: bind lost")
	webErr := make(chan error, 1)
	webErr <- wantErr

	done := make(chan error, 1)
	go func() {
		_, err := serveUntilSignal(sigCtx, context.Background(), io.Discard, io.Discard, nil, s, 0, webErr, nil)
		done <- err
	}()

	select {
	case got := <-done:
		if !errors.Is(got, wantErr) {
			t.Fatalf("serveUntilSignal returned %v, want %v", got, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveUntilSignal did not return after the web surface failed — the error was swallowed")
	}
}
