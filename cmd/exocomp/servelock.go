package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"exocomp.dev/exocomp/internal/store"
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

// probeGrace bounds how long acquiring keeps trying. A daemon that really owns
// the dir holds the lock for its whole life, so this only ever delays a refusal
// — but a CLI probing the same file holds a shared lock for an instant, and
// losing the daemon to that instant is not a thing to accept.
const (
	probeGrace = 250 * time.Millisecond
	probeRetry = 10 * time.Millisecond
)

// acquireServeLock takes the data dir's daemon lock, and records this process's
// pid in it so a second daemon can name the holder.
func acquireServeLock(dir string) (*serveLock, error) {
	if err := os.MkdirAll(dir, dataDirMode); err != nil {
		return nil, fmt.Errorf("serve: create data dir: %w", err)
	}

	path := filepath.Join(dir, serveLockName)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("serve: open %s: %w", path, err)
	}

	if err := lockExclusive(file); err != nil {
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

// lockExclusive takes the lock without blocking, retrying for probeGrace so a
// probe's momentary shared lock does not cost a starting daemon the dir.
func lockExclusive(file *os.File) error {
	deadline := time.Now().Add(probeGrace)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(probeRetry)
	}
}

// serveLockHeld reports whether a daemon holds dir's lock. It is how a CLI
// tells a running daemon from the short-lived state backend another CLI
// started: both answer /healthz, only the daemon drains the queue.
//
// The probe is shared, never exclusive: asking the question must not answer it
// for anyone else. An exclusive probe conflicts with every other probe, so two
// CLIs asking at once each conclude a daemon is up.
func serveLockHeld(dir string) bool {
	file, err := os.Open(filepath.Join(dir, serveLockName))
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()

	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	// Nobody holds it exclusively; release the shared lock rather than keeping
	// what this process has no business keeping.
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}

// The cross-host active lease sits above the host-local flock. serve.lock stops
// two daemons on ONE host but is meaningless across hosts sharing one replicated
// store — a Litestream/restored copy, a shared volume — where two daemons could
// each go active and split-brain the queue. The lease row in that shared store
// is the missing cross-host guard: the active daemon holds and renews it, and a
// starting daemon that finds it live refuses rather than double-activate. Both
// guards stay: the flock is the cheap same-host answer, the lease the cross-host
// one.

// leaseHolderID names this daemon in the shared lease. Hostname and pid make it
// legible to the operator a refusal names, and distinguish two hosts sharing one
// store. Two daemons on one host share a hostname, but the flock stops the second
// before the lease is consulted, so the pid only ever disambiguates them in a log.
func leaseHolderID() string {
	return fmt.Sprintf("%s/pid-%d", leaseHostname(), os.Getpid())
}

func leaseHostname() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown-host"
	}
	return host
}

// leaseHost is the host portion of a holder id. A lease still recorded to THIS
// host is a dead predecessor's: the flock this process already holds proves no
// live daemon on this host, so the row was left by a crash that could not
// release it, and a restart may take it over without waiting out the TTL. Only a
// lease held by ANOTHER host is the cross-host fence the TTL and --force-lease
// govern.
func leaseHost(holderID string) string {
	host, _, _ := strings.Cut(holderID, "/pid-")
	return host
}

// serveLease is a held cross-host lease. renewUntil keeps it live; release frees
// it on a clean stop so a standby need not wait out the TTL.
type serveLease struct {
	store    *store.Store
	holderID string
	ttl      time.Duration
	renew    time.Duration
}

// acquireLease takes the cross-host single-active lease. When another daemon's
// lease is still live it refuses with a message naming the holder and when it was
// last renewed — the split-brain the flock cannot see. force takes over a live
// lease: the operator's explicit override for a holder known to be gone.
func acquireLease(ctx context.Context, st *store.Store, holderID string, ttl, renew time.Duration, force bool, out io.Writer) (*serveLease, error) {
	now := time.Now().UTC()

	// A lease left by a same-host predecessor is taken over unconditionally: the
	// flock already proves that daemon is gone, so a crash-restart must not be
	// held off for a whole TTL. force keeps its meaning for a DIFFERENT host.
	//
	// sameHost is handed to AcquireLease rather than decided here from a
	// GetLease read of our own: evaluated inside AcquireLease's own transaction
	// against the row it just read, it can never go stale the way a read taken
	// here and acted on afterward could — closing the gap where a fast restart
	// might otherwise clobber a live lease a different host took in between.
	sameHost := func(currentHolderID string) bool {
		return leaseHost(currentHolderID) == leaseHost(holderID)
	}

	lease, err := st.AcquireLease(ctx, holderID, ttl, now, force, sameHost)
	if errors.Is(err, store.ErrLeaseHeld) {
		ago := now.Sub(lease.RenewedAt).Round(time.Second)
		return nil, fmt.Errorf(
			"serve: the active lease is held by %q (last renewed %s, %s ago; ttl %s); "+
				"refusing to become active — another daemon is serving this store. "+
				"Pass --force-lease only once you are certain that holder is gone",
			lease.HolderID, lease.RenewedAt.Format(time.RFC3339), ago, lease.TTL)
	}
	if err != nil {
		return nil, fmt.Errorf("serve: acquire active lease: %w", err)
	}
	if force {
		_, _ = fmt.Fprintf(out, "WARNING: force-took the active lease; the previous holder must not be running\n")
	}
	_, _ = fmt.Fprintf(out, "active lease acquired (holder %q, ttl %s)\n", holderID, ttl)
	return &serveLease{store: st, holderID: holderID, ttl: ttl, renew: renew}, nil
}

// renewUntil refreshes the lease on its period until ctx ends. Losing it to a
// force-takeover is terminal: it calls standDown so this daemon begins the same
// graceful stop a signal would — being fenced out means it is no longer active —
// and returns. renew must stay well below ttl so a slow write does not let the
// lease lapse under a daemon that is still serving.
func (l *serveLease) renewUntil(ctx context.Context, standDown func(), warn io.Writer) {
	ticker := time.NewTicker(l.renew)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := l.store.RenewLease(ctx, l.holderID, time.Now().UTC())
		if errors.Is(err, store.ErrLeaseLost) {
			_, _ = fmt.Fprintf(warn, "WARNING: active lease taken over by another daemon; standing down\n")
			standDown()
			return
		}
		if err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintf(warn, "WARNING: renew active lease: %v\n", err)
		}
	}
}

// release frees the lease on a clean stop. It takes its own context because the
// daemon's is already cancelled by the time shutdown runs, and a cancelled
// context would turn the freeing write into a no-op that strands the fence for a
// standby until the TTL lapses.
func (l *serveLease) release(ctx context.Context, warn io.Writer) {
	if err := l.store.ReleaseLease(ctx, l.holderID); err != nil {
		_, _ = fmt.Fprintf(warn, "WARNING: release active lease: %v\n", err)
	}
}
