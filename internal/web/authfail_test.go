package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestBearerFailuresBackOff is the AI-11 regression: 200 consecutive wrong-secret
// requests used to draw 200 plain 401s — no throttle, no backoff, no lockout,
// nothing logged — because the only pre-auth control was the KDF concurrency
// gate, a ceiling on parallelism rather than on rate. Repeated failures now back
// off, and the backoff lifts on its own once the window passes.
func TestBearerFailuresBackOff(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.server.clock = func() time.Time { return now }
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	good := h.mint(t, "op", "operator")
	id, _, err := parseToken(good)
	if err != nil {
		t.Fatalf("parseToken: %v", err)
	}
	wrong := "exo_" + strconv.FormatInt(id, 10) + "_" + strings.Repeat("a", 32)

	throttled := 0
	for i := 0; i < authFailThreshold+3; i++ {
		code, hdr, _ := bearerGet(t, ts.URL, "/api/v1/jobs", wrong)
		switch code {
		case http.StatusUnauthorized:
		case http.StatusTooManyRequests:
			throttled++
			if hdr.Get("Retry-After") == "" {
				t.Error("a throttled response carries no Retry-After")
			}
		default:
			t.Fatalf("attempt %d status = %d, want 401 or 429", i, code)
		}
	}
	if throttled == 0 {
		t.Fatalf("%d consecutive bad secrets drew no throttle at all", authFailThreshold+3)
	}

	// The backoff is a window, not a permanent lockout: past it the real token
	// authenticates again.
	now = now.Add(authFailMaxBackoff + time.Second)
	if code, _, body := bearerGet(t, ts.URL, "/api/v1/jobs", good); code != http.StatusOK {
		t.Fatalf("post-backoff read status = %d, want 200 (%s)", code, body)
	}
}

// TestAbsentTokenRunsNoKDF pins the S1 simplification: the timing equalizer
// (the decoy argon2id an unknown/inactive row used to pay so it looked like a
// live one) is gone. An absent id now returns unauthenticated WITHOUT running
// the KDF, while a live row's verification does run the real KDF. The resulting
// live-vs-absent timing gap is accepted (finding AI-11 disposition): the fail
// counter and KDF gate — not a decoy — bound an unauthenticated guessing run.
func TestAbsentTokenRunsNoKDF(t *testing.T) {
	h := newHarness(t)
	var kdfRuns atomic.Int64
	underlying := h.server.verify
	h.server.verify = func(secret, phc string) (bool, error) {
		kdfRuns.Add(1)
		return underlying(secret, phc)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	live := h.mint(t, "op", "operator")
	id, liveSecret, err := parseToken(live)
	if err != nil {
		t.Fatalf("parseToken: %v", err)
	}
	wrong := strings.Repeat("b", 32)

	// An unknown id: no row, so no decoy KDF. This is the timing gap we accept.
	kdfRuns.Store(0)
	if code, _, _ := bearerGet(t, ts.URL, "/api/v1/jobs",
		"exo_999999_"+wrong); code != http.StatusUnauthorized {
		t.Fatal("an unknown token id must be 401")
	}
	if n := kdfRuns.Swap(0); n != 0 {
		t.Fatalf("an absent token ran the KDF %d time(s); the equalizer decoy should be gone", n)
	}

	// A live id with a wrong secret: the real verification path runs, so exactly
	// one KDF fires and the compare fails.
	if code, _, _ := bearerGet(t, ts.URL, "/api/v1/jobs",
		"exo_"+strconv.FormatInt(id, 10)+"_"+wrong); code != http.StatusUnauthorized {
		t.Fatal("a wrong secret for a live token must be 401")
	}
	if n := kdfRuns.Swap(0); n != 1 {
		t.Fatalf("a live row's wrong-secret check ran the KDF %d time(s), want 1", n)
	}

	// A genuine live verification runs the real KDF and succeeds.
	if code, _, body := bearerGet(t, ts.URL, "/api/v1/jobs",
		"exo_"+strconv.FormatInt(id, 10)+"_"+liveSecret); code != http.StatusOK {
		t.Fatalf("a correct secret must authenticate; status = %d (%s)", code, body)
	}
	if n := kdfRuns.Swap(0); n != 1 {
		t.Fatalf("a genuine login ran the KDF %d time(s), want 1", n)
	}
}

// TestGuessingRunRefusedBeforeKDF is the blue-team proof that removing the
// equalizer did not weaken the guard that actually bounds guessing: once a
// source has failed past the threshold, further attempts are refused with 429
// BEFORE the KDF runs. This holds independently of whether the presented id is
// live or absent — the fail counter keys on the source, not on row existence —
// so an unauthenticated guessing run is refused pre-KDF either way.
func TestGuessingRunRefusedBeforeKDF(t *testing.T) {
	run := func(t *testing.T, useLiveID bool) {
		h := newHarness(t)
		now := time.Now().UTC()
		h.server.clock = func() time.Time { return now }
		var kdfRuns atomic.Int64
		underlying := h.server.verify
		h.server.verify = func(secret, phc string) (bool, error) {
			kdfRuns.Add(1)
			return underlying(secret, phc)
		}
		ts := httptest.NewServer(h.server.Handler())
		defer ts.Close()

		id := int64(999999)
		if useLiveID {
			live := h.mint(t, "op", "operator")
			parsedID, _, err := parseToken(live)
			if err != nil {
				t.Fatalf("parseToken: %v", err)
			}
			id = parsedID
		}
		wrong := "exo_" + strconv.FormatInt(id, 10) + "_" + strings.Repeat("a", 32)

		throttled := false
		for i := 0; i < authFailThreshold+4; i++ {
			before := kdfRuns.Load()
			code, hdr, _ := bearerGet(t, ts.URL, "/api/v1/jobs", wrong)
			after := kdfRuns.Load()
			switch code {
			case http.StatusUnauthorized:
			case http.StatusTooManyRequests:
				throttled = true
				if hdr.Get("Retry-After") == "" {
					t.Error("a throttled response carries no Retry-After")
				}
				// The whole point: a refused attempt spends no KDF.
				if after != before {
					t.Fatalf("attempt %d was throttled yet ran the KDF (%d -> %d) — the guard did not refuse before the KDF",
						i, before, after)
				}
			default:
				t.Fatalf("attempt %d status = %d, want 401 or 429", i, code)
			}
		}
		if !throttled {
			t.Fatalf("%d consecutive failures drew no throttle at all", authFailThreshold+4)
		}
		// The KDF ran at most until the backoff engaged, never once per attempt.
		// An absent id never runs it at all; a live id runs it only until the
		// threshold refuses further attempts pre-KDF.
		if total := kdfRuns.Load(); total > int64(authFailThreshold+1) {
			t.Fatalf("KDF ran %d times across a guessing run, want <= %d — the fail counter did not bound it",
				total, authFailThreshold+1)
		}
	}

	t.Run("live id", func(t *testing.T) { run(t, true) })
	t.Run("absent id", func(t *testing.T) { run(t, false) })
}
