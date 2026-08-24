package secrets

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// setMemlockRlimitZero lowers this process's soft RLIMIT_MEMLOCK to 0 for the
// duration of the test, and returns a func that restores it. It only ever
// lowers Cur, never Max: raising Max back up needs CAP_SYS_RESOURCE, which an
// unprivileged test process does not have, but raising Cur back to the
// untouched Max never does.
func setMemlockRlimitZero(t *testing.T) func() {
	t.Helper()
	var rlim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &rlim); err != nil {
		t.Skipf("getrlimit RLIMIT_MEMLOCK: %v", err)
	}
	if rlim.Max == 0 {
		t.Skip("RLIMIT_MEMLOCK max is already 0; nothing to lower")
	}
	original := rlim
	zero := unix.Rlimit{Cur: 0, Max: rlim.Max}
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &zero); err != nil {
		t.Skipf("setrlimit RLIMIT_MEMLOCK=0: %v", err)
	}
	return func() {
		if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &original); err != nil {
			t.Errorf("restore RLIMIT_MEMLOCK: %v", err)
		}
	}
}

// vmLockedKB reads the process's locked-memory size (VmLck) from
// /proc/self/status, in kilobytes. It is how the E5 repro observes whether the
// DEK sits in ordinary swappable, core-dumpable memory (VmLck unchanged) or in
// locked memory (VmLck grows).
func vmLockedKB(t *testing.T) int {
	t.Helper()
	body, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmLck:"); ok {
			rest = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB"))
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				t.Fatalf("parse VmLck %q: %v", rest, err)
			}
			return n
		}
	}
	t.Skip("no VmLck line in /proc/self/status")
	return 0
}

// TestInitLocksTheDEK is the E5 repro: the DEK must live in mlocked memory, not
// ordinary heap that can be swapped to disk or written to a core dump. Before
// the fix Init locks nothing and MemoryLock().Locked is false.
func TestInitLocksTheDEK(t *testing.T) {
	env, _ := mustInit(t, t.TempDir(), nil)

	if lock := env.MemoryLock(); !lock.Locked {
		t.Fatalf("DEK is not mlocked after Init: %v", lock.Err)
	}
	// A locked DEK means at least one page is pinned, so the kernel's own
	// accounting must report a non-zero locked set. This is an absolute floor
	// rather than a before/after delta: mlock is page-granular, so a 32-byte
	// DEK sharing a page with an already-locked buffer would move no delta even
	// though it is locked.
	if kb := vmLockedKB(t); kb <= 0 {
		t.Errorf("VmLck is %d kB after Init, want a locked page", kb)
	}
}

// TestCloseUnlocksTheDEK proves Close releases the lock it took, so a
// long-running daemon that opens and closes envelopes does not leak locked
// pages against RLIMIT_MEMLOCK.
func TestCloseUnlocksTheDEK(t *testing.T) {
	env, _, err := Init(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !env.MemoryLock().Locked {
		t.Fatalf("DEK not locked after Init")
	}
	if err := env.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if env.MemoryLock().Locked {
		t.Error("MemoryLock still reports locked after Close")
	}
}

// TestLockBytesLocksAnExternalBuffer proves the exported wrapper a caller
// outside this package uses (the executor's /dev/shm run-secret staging) pins
// real pages, the same as the DEK's own lock.
func TestLockBytesLocksAnExternalBuffer(t *testing.T) {
	before := vmLockedKB(t)

	b := make([]byte, os.Getpagesize())
	lock := LockBytes(b)
	if !lock.Locked {
		t.Fatalf("LockBytes did not lock: %v", lock.Err)
	}
	if kb := vmLockedKB(t); kb <= before {
		t.Errorf("VmLck did not grow after LockBytes: before=%d after=%d", before, kb)
	}

	if err := UnlockBytes(b); err != nil {
		t.Fatalf("UnlockBytes: %v", err)
	}
}

// TestLockBytesFailsOpenOverLimit proves the best-effort contract: with
// RLIMIT_MEMLOCK set to 0, LockBytes reports a failure rather than panicking
// or blocking, so a caller can proceed unlocked with the residual reported —
// exactly the fallback the /dev/shm staging path relies on.
func TestLockBytesFailsOpenOverLimit(t *testing.T) {
	restore := setMemlockRlimitZero(t)
	defer restore()

	b := make([]byte, os.Getpagesize())
	lock := LockBytes(b)
	if lock.Locked {
		t.Fatal("LockBytes locked despite RLIMIT_MEMLOCK=0")
	}
	if lock.Err == nil {
		t.Error("LockBytes reported no error for the failure")
	}
}
