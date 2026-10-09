package main

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/sched"
)

// TestReloadRefreshesExtraSet is the read-once audit's seam: `exocomp reload`
// (and SIGHUP) rebuilds not only the scheduler registrations but every extra
// read-once set registered with setReloadExtra — the notify transports.
func TestReloadRefreshesExtraSet(t *testing.T) {
	st := restartTestStore(t)
	s := sched.New(st, nil, nil)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	c := newCordonController(st, nil)
	c.setScheduler(s)

	var refreshes atomic.Int32
	c.setReloadExtra(func(context.Context) error {
		refreshes.Add(1)
		return nil
	})

	got, err := c.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got.Schedules != 0 {
		t.Fatalf("Reload reported %d schedules, want 0", got.Schedules)
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("the extra reload ran %d times, want 1", n)
	}
}

// TestReloadExtraFailurePropagates: a refused extra refresh fails the reload
// rather than reporting success, so an operator sees that the push config did
// not take. The scheduler half has already refreshed; the extra set is left as
// it was by its own all-or-nothing rebuild.
func TestReloadExtraFailurePropagates(t *testing.T) {
	st := restartTestStore(t)
	s := sched.New(st, nil, nil)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	defer s.Stop()

	c := newCordonController(st, nil)
	c.setScheduler(s)

	want := errors.New("notify: refused")
	c.setReloadExtra(func(context.Context) error { return want })

	if _, err := c.Reload(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Reload error = %v, want %v", err, want)
	}
}

// TestServeUntilSignalReloadsOnHUP proves the signal path drives the same seam
// as the operator API: SIGHUP calls the injected reload, so a push config a
// SIGHUP is meant to pick up is not silently left to the operator `reload` verb
// alone.
func TestServeUntilSignalReloadsOnHUP(t *testing.T) {
	sigCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hup := make(chan os.Signal, 1)
	var reloads atomic.Int32
	reload := func(context.Context) error {
		reloads.Add(1)
		return nil
	}

	done := make(chan bool, 1)
	go func() {
		restarting, _ := serveUntilSignal(sigCtx, context.Background(), io.Discard, io.Discard,
			hup, reload, 0, nil, nil)
		done <- restarting
	}()

	hup <- syscall.SIGHUP
	waitFor(t, "SIGHUP to drive a reload", 2*time.Second, func() bool {
		return reloads.Load() == 1
	})

	cancel()
	select {
	case restarting := <-done:
		if restarting {
			t.Fatal("a signal stop reported a restart")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveUntilSignal did not return after SIGINT")
	}
}
