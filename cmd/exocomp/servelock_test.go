package main

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// sharedLock takes a shared lock on the data dir's lock file from a descriptor
// of its own, standing in for another process's probe, and returns the release.
func sharedLock(t *testing.T, dir string) func() {
	t.Helper()

	file, err := os.OpenFile(filepath.Join(dir, serveLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatalf("take a shared lock: %v", err)
	}
	return func() { _ = file.Close() }
}

func TestServeLockHeldFollowsTheHolder(t *testing.T) {
	dir := t.TempDir()

	if serveLockHeld(dir) {
		t.Error("an untouched data dir reports a running daemon")
	}

	lock, err := acquireServeLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !serveLockHeld(dir) {
		t.Error("a held lock does not report a running daemon")
	}

	lock.release()
	if serveLockHeld(dir) {
		t.Error("a released lock still reports a running daemon")
	}
}

// TestServeLockProbeIsNotExclusive: another probe's shared lock is not a
// daemon. An exclusive probe cannot tell the two apart, and answers "a daemon
// is up" to a data dir nothing is serving.
func TestServeLockProbeIsNotExclusive(t *testing.T) {
	dir := t.TempDir()

	release := sharedLock(t, dir)
	defer release()

	if serveLockHeld(dir) {
		t.Error("a shared lock was mistaken for a running daemon")
	}
}

// TestConcurrentProbesDoNotSeeEachOther: probing must not answer the question
// for the next caller. Exclusive probes conflict with one another, so several
// CLIs asking at once each conclude a daemon is running.
func TestConcurrentProbesDoNotSeeEachOther(t *testing.T) {
	dir := t.TempDir()
	if _, err := os.Create(filepath.Join(dir, serveLockName)); err != nil {
		t.Fatalf("create lock file: %v", err)
	}

	const probers, rounds = 8, 200
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wrong int
	)
	for range probers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				if serveLockHeld(dir) {
					mu.Lock()
					wrong++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if wrong != 0 {
		t.Errorf("%d probe(s) reported a daemon that does not exist", wrong)
	}
}

// TestAcquireOutlastsAProbe: a daemon starting while a CLI probes must get the
// dir. The probe's lock is shared and momentary; refusing on the first attempt
// hands the loss to whoever started at the wrong microsecond.
func TestAcquireOutlastsAProbe(t *testing.T) {
	dir := t.TempDir()

	release := sharedLock(t, dir)
	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()

	lock, err := acquireServeLock(dir)
	if err != nil {
		t.Fatalf("a daemon lost the data dir to a probe: %v", err)
	}
	lock.release()
}
