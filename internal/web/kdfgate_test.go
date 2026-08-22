package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestKDFGatePerSourceCap proves one source cannot hold more than perSource
// slots: the third concurrent acquire from a source already holding its cap is
// refused without running the KDF, and a slot frees when one is released.
func TestKDFGatePerSourceCap(t *testing.T) {
	t.Parallel()
	g := newKDFGate()
	g.wait = 50 * time.Millisecond

	r1, ok := g.acquire("src")
	if !ok {
		t.Fatal("first acquire refused")
	}
	r2, ok := g.acquire("src")
	if !ok {
		t.Fatal("second acquire refused")
	}
	if _, ok := g.acquire("src"); ok {
		t.Fatal("third acquire from a capped source must be refused")
	}
	r1()
	r3, ok := g.acquire("src")
	if !ok {
		t.Fatal("acquire after a release refused")
	}
	r2()
	r3()
}

// TestKDFGateGlobalCap proves the global ceiling: with every slot held by a
// distinct source (each below its per-source cap), a further source is refused
// rather than adding another concurrent KDF.
func TestKDFGateGlobalCap(t *testing.T) {
	t.Parallel()
	g := newKDFGate()
	g.wait = 50 * time.Millisecond

	var releases []func()
	for i := 0; i < g.capacity(); i++ {
		r, ok := g.acquire(fmt.Sprintf("src-%d", i))
		if !ok {
			t.Fatalf("acquire %d of %d refused", i, g.capacity())
		}
		releases = append(releases, r)
	}
	if _, ok := g.acquire("one-more"); ok {
		t.Fatal("a source beyond the global ceiling must be refused")
	}
	for _, r := range releases {
		r()
	}
	if r, ok := g.acquire("after"); !ok {
		t.Fatal("acquire after releasing every slot refused")
	} else {
		r()
	}
}

// TestKDFGateBoundsConcurrentWork drives the gate the way a flood does — many
// goroutines across many sources — and asserts the number of slots held at once
// never exceeds the global ceiling. This is the memory bound finding D1 needs:
// argon2id runs only while a slot is held, so a bounded slot count is a bounded
// number of concurrent 64 MiB allocations.
func TestKDFGateBoundsConcurrentWork(t *testing.T) {
	t.Parallel()
	g := newKDFGate()

	var cur, peak int64
	var wg sync.WaitGroup
	for s := 0; s < 30; s++ {
		for a := 0; a < 4; a++ {
			wg.Add(1)
			go func(source string) {
				defer wg.Done()
				release, ok := g.acquire(source)
				if !ok {
					return
				}
				defer release()
				c := atomic.AddInt64(&cur, 1)
				for {
					p := atomic.LoadInt64(&peak)
					if c <= p || atomic.CompareAndSwapInt64(&peak, p, c) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt64(&cur, -1)
			}(fmt.Sprintf("src-%d", s))
		}
	}
	wg.Wait()

	if peak > int64(g.capacity()) {
		t.Fatalf("peak concurrent KDF slots = %d, exceeds the ceiling %d", peak, g.capacity())
	}
	t.Logf("peak concurrent slots %d, ceiling %d", peak, g.capacity())
}

// TestLoginKDFNotAmplified is finding D1 at the HTTP boundary: a flood of
// unauthenticated bearer requests that each land on a genuine token row (so the
// argon2id verify would run) is bounded by the gate — the KDF is never run
// unboundedly in parallel, and the excess is refused with 503 having run no
// KDF at all. A genuine login still succeeds.
func TestLoginKDFNotAmplified(t *testing.T) {
	h := newHarness(t)

	// Observe the KDF: count how many verifications run at once and in total,
	// while still doing the genuine check so a correct token authenticates.
	var cur, peak, total int64
	h.server.verify = func(secret, phc string) (bool, error) {
		c := atomic.AddInt64(&cur, 1)
		atomic.AddInt64(&total, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if c <= p || atomic.CompareAndSwapInt64(&peak, p, c) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt64(&cur, -1)
		return verifySecret(secret, phc)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// A genuine token row, so a bogus bearer against its id makes the verify run
	// (an unknown id would skip the KDF and prove nothing about the gate).
	genuine := h.mint(t, "laptop", "operator")
	id, _, err := parseToken(genuine)
	if err != nil {
		t.Fatalf("parseToken: %v", err)
	}
	bogus := fmt.Sprintf("%s_%d_%s", tokenScheme, id, "not-the-secret-000000000000")

	const flood = 40
	var got503, gotAuthFail int64
	var wg sync.WaitGroup
	for i := 0; i < flood; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
			req.Header.Set("Authorization", "Bearer "+bogus)
			resp, err := ts.Client().Do(req)
			if err != nil {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			switch resp.StatusCode {
			case http.StatusServiceUnavailable:
				atomic.AddInt64(&got503, 1)
			case http.StatusUnauthorized:
				atomic.AddInt64(&gotAuthFail, 1)
			}
		}()
	}
	wg.Wait()

	if peak > int64(h.server.kdf.capacity()) {
		t.Fatalf("peak concurrent KDF runs = %d, exceeds the ceiling %d — the flood amplified the KDF",
			peak, h.server.kdf.capacity())
	}
	if total >= flood {
		t.Fatalf("the KDF ran %d times for %d requests — the flood was not bounded", total, flood)
	}
	if got503 == 0 {
		t.Fatal("no request was shed with 503; the gate never refused the flood")
	}
	t.Logf("flood of %d: KDF ran %d times, peak %d (ceiling %d), %d shed with 503, %d rejected as auth failures",
		flood, total, peak, h.server.kdf.capacity(), got503, gotAuthFail)

	// The gate does not break a genuine login.
	drained := false
	for !drained {
		if atomic.LoadInt64(&cur) == 0 {
			drained = true
		}
		time.Sleep(5 * time.Millisecond)
	}
	client := login(t, ts.URL, genuine)
	if code, _ := getBody(t, client, ts.URL+"/"); code != http.StatusOK {
		t.Fatalf("a genuine login after the flood got %d, want 200", code)
	}
}
