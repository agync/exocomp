package web

import (
	"sync"
	"time"
)

// The KDF gate (kdfgate.go) bounds how many argon2id evaluations run at once. It
// is explicitly not a rate limit, so a patient attacker could present wrong
// secrets forever — 200 consecutive failures drew 200 plain 401s, with no
// throttle, no lockout and nothing recorded. This file adds the missing half:
// consecutive failures are counted per source and per token row, and past a
// threshold the source is backed off for a growing window and the run is
// logged loudly.

// authFailThreshold is how many consecutive failures one key may draw before the
// backoff starts. It is generous enough that an operator fat-fingering a paste a
// few times never notices.
const authFailThreshold = 5

// authFailBase and authFailMaxBackoff bound the backoff window: it doubles per
// failure past the threshold and stops at the maximum, so a guessing run is
// throttled to a crawl while a genuine caller is never locked out for long.
const (
	authFailBase       = 2 * time.Second
	authFailMaxBackoff = 5 * time.Minute
)

// authFailIdle drops an entry that has not failed for this long, so the map
// cannot grow without bound on a long-lived daemon.
const authFailIdle = 30 * time.Minute

type failEntry struct {
	fails int
	until time.Time
	seen  time.Time
}

// failGate counts consecutive authentication failures per key and reports the
// backoff a key is currently serving.
type failGate struct {
	mu      sync.Mutex
	entries map[string]*failEntry
}

func newFailGate() *failGate { return &failGate{entries: make(map[string]*failEntry)} }

// blocked reports the remaining backoff for the worst of the given keys.
func (g *failGate) blocked(now time.Time, keys ...string) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var worst time.Duration
	for _, k := range keys {
		e := g.entries[k]
		if e == nil {
			continue
		}
		if d := e.until.Sub(now); d > worst {
			worst = d
		}
	}
	return worst, worst > 0
}

// fail records one failure against every key and returns the highest
// consecutive-failure count reached, so the caller can log a guessing run.
func (g *failGate) fail(now time.Time, keys ...string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(now)
	worst := 0
	for _, k := range keys {
		e := g.entries[k]
		if e == nil {
			e = &failEntry{}
			g.entries[k] = e
		}
		e.fails++
		e.seen = now
		if e.fails > authFailThreshold {
			e.until = now.Add(backoffFor(e.fails))
		}
		if e.fails > worst {
			worst = e.fails
		}
	}
	return worst
}

// succeed clears the counters for keys behind a successful authentication.
func (g *failGate) succeed(keys ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range keys {
		delete(g.entries, k)
	}
}

// sweep drops entries idle past authFailIdle. It runs under the caller's lock.
func (g *failGate) sweep(now time.Time) {
	for k, e := range g.entries {
		if now.Sub(e.seen) > authFailIdle && !now.Before(e.until) {
			delete(g.entries, k)
		}
	}
}

// backoffFor doubles per failure past the threshold, capped.
func backoffFor(fails int) time.Duration {
	d := authFailBase
	for i := authFailThreshold + 1; i < fails; i++ {
		d *= 2
		if d >= authFailMaxBackoff {
			return authFailMaxBackoff
		}
	}
	return d
}
