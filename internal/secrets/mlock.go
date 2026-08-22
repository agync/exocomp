package secrets

import "golang.org/x/sys/unix"

// MemoryLock reports whether a secret buffer was pinned into RAM with mlock(2),
// keeping it out of swap and core dumps, and why not when it was not. It is
// what `exocomp doctor` reads to state the real posture rather than the ADR's
// claim (finding E5).
type MemoryLock struct {
	Locked bool
	// Err is the reason locking was not in effect: the mlock failure on a
	// host that forbids it (RLIMIT_MEMLOCK), or nil once Close has released a
	// lock it held.
	Err error
}

// lockMemory pins b's pages into RAM. A failure is returned rather than fatal:
// an unprivileged container with a low RLIMIT_MEMLOCK still runs, just without
// this defence, which doctor then reports honestly.
func lockMemory(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return unix.Mlock(b)
}

// unlockMemory releases a pin taken by lockMemory. Called from Close so a
// daemon that opens and closes envelopes does not accumulate locked pages.
func unlockMemory(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return unix.Munlock(b)
}

// ProbeMemoryLock reports whether this process can mlock secret memory, without
// opening the envelope: it locks and immediately unlocks a probe page. It is
// what doctor reads on a host that is not yet initialized, so the mlock posture
// is visible before there is a DEK to lock.
func ProbeMemoryLock() MemoryLock {
	probe := make([]byte, dekLen)
	if err := lockMemory(probe); err != nil {
		return MemoryLock{Locked: false, Err: err}
	}
	_ = unlockMemory(probe)
	return MemoryLock{Locked: true}
}
