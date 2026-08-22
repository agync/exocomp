package web

import (
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// kdfGate bounds concurrent argon2id evaluations on the pre-auth path. argon2id
// is memory-hard by design — one verification allocates argonMemory (64 MiB) —
// so an unauthenticated flood of /login or bearer requests, each triggering a
// KDF, multiplies that into real memory pressure (finding D1). The gate caps
// how many KDFs run at once (a hard ceiling on the memory the KDF can hold) and
// how many one source may hold at once (so a single flooding source cannot
// starve a genuine login). A caller that cannot get a slot never runs the KDF,
// so a rejected flood costs no memory and frees slots immediately; a real login
// waits at most kdfWait and, because KDFs are tens of milliseconds, gets one.
type kdfGate struct {
	global    chan struct{}
	wait      time.Duration
	perSource int

	mu    sync.Mutex
	inUse map[string]int
}

// kdfWait bounds how long a login blocks for a KDF slot before being told to
// retry. It is comfortably longer than a single argon2id evaluation, so under
// normal load a slot is always free and the wait is never reached.
const kdfWait = 2 * time.Second

func newKDFGate() *kdfGate {
	slots := runtime.GOMAXPROCS(0)
	if slots < 2 {
		slots = 2
	}
	if slots > 8 {
		slots = 8
	}
	return &kdfGate{
		global:    make(chan struct{}, slots),
		wait:      kdfWait,
		perSource: 2,
		inUse:     make(map[string]int),
	}
}

// capacity is the hard ceiling on concurrent KDF evaluations. The regression
// test asserts observed concurrency never exceeds it.
func (g *kdfGate) capacity() int { return cap(g.global) }

// acquire reserves a KDF slot for source, or reports the gate is saturated
// without running the KDF. When it returns true the caller MUST call release
// exactly once; when false it must not run the KDF at all.
func (g *kdfGate) acquire(source string) (release func(), ok bool) {
	// Reserve a per-source slot first, so one source can never hold every
	// global slot. A source already at its cap is rejected before it can even
	// queue for the global semaphore.
	g.mu.Lock()
	if g.inUse[source] >= g.perSource {
		g.mu.Unlock()
		return nil, false
	}
	g.inUse[source]++
	g.mu.Unlock()

	releaseSource := func() {
		g.mu.Lock()
		if g.inUse[source] <= 1 {
			delete(g.inUse, source)
		} else {
			g.inUse[source]--
		}
		g.mu.Unlock()
	}

	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	select {
	case g.global <- struct{}{}:
		return func() { <-g.global; releaseSource() }, true
	case <-timer.C:
		releaseSource()
		return nil, false
	}
}

// sourceKey identifies the origin of a request for the per-source cap. It is
// the peer address only: X-Forwarded-For is deliberately ignored, since a
// spoofed header would let a single attacker present as many sources (the
// global cap still bounds total memory regardless — finding D4 tracks the
// proxy case separately).
func sourceKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
