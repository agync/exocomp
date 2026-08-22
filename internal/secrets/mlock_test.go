package secrets

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

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
