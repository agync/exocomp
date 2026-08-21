package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// serveLockName is the file a running daemon holds an exclusive lock on, in
// the data dir it serves. One operator means one daemon per data dir
// (AGENTS.md invariant 1): two of them fire every schedule twice and race for
// every queued job.
const serveLockName = "serve.lock"

// serveLock is a held daemon lock. The lock is the open descriptor's, so it is
// released by closing the file and by the process ending, however it ends —
// there is no stale lock file to clean up after a kill.
type serveLock struct{ file *os.File }

// acquireServeLock takes the data dir's daemon lock without blocking, and
// records this process's pid in it so a second daemon can name the holder.
func acquireServeLock(dir string) (*serveLock, error) {
	if err := os.MkdirAll(dir, dataDirMode); err != nil {
		return nil, fmt.Errorf("serve: create data dir: %w", err)
	}

	path := filepath.Join(dir, serveLockName)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("serve: open %s: %w", path, err)
	}

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := lockHolder(file)
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("serve: another exocomp serve (pid %s) already owns %s", holder, dir)
		}
		return nil, fmt.Errorf("serve: lock %s: %w", path, err)
	}

	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("serve: write %s: %w", path, err)
	}
	if _, err := file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("serve: write %s: %w", path, err)
	}
	return &serveLock{file: file}, nil
}

func (l *serveLock) release() { _ = l.file.Close() }

// lockHolder reads the pid a daemon recorded, for the message the refused
// second daemon prints. An unreadable pid is not a reason to start anyway.
func lockHolder(file *os.File) string {
	raw := make([]byte, 32)
	n, err := file.ReadAt(raw, 0)
	if n == 0 && err != nil {
		return "unknown"
	}
	pid := strings.TrimSpace(string(raw[:n]))
	if pid == "" {
		return "unknown"
	}
	return pid
}

// serveLockHeld reports whether a daemon holds dir's lock. It is how a CLI
// tells a running daemon from the short-lived state backend another CLI
// started: both answer /healthz, only the daemon drains the queue.
func serveLockHeld(dir string) bool {
	file, err := os.OpenFile(filepath.Join(dir, serveLockName), os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	// The lock was free; release it again rather than holding what this
	// process has no business holding.
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}
