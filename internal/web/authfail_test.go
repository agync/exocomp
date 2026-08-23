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

// TestUnknownTokenPaysTheKDF is the second half of AI-11: authenticateRow
// returned immediately for an unknown or inactive row but paid a full argon2id
// for a live one, so response time told an attacker which token ids exist. Both
// paths now run the KDF.
func TestUnknownTokenPaysTheKDF(t *testing.T) {
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
	id, _, err := parseToken(live)
	if err != nil {
		t.Fatalf("parseToken: %v", err)
	}
	secret := strings.Repeat("b", 32)

	kdfRuns.Store(0)
	if code, _, _ := bearerGet(t, ts.URL, "/api/v1/jobs",
		"exo_"+strconv.FormatInt(id, 10)+"_"+secret); code != http.StatusUnauthorized {
		t.Fatal("a wrong secret for a live token must be 401")
	}
	existing := kdfRuns.Swap(0)

	if code, _, _ := bearerGet(t, ts.URL, "/api/v1/jobs",
		"exo_999999_"+secret); code != http.StatusUnauthorized {
		t.Fatal("an unknown token id must be 401")
	}
	missing := kdfRuns.Swap(0)

	if existing != missing {
		t.Fatalf("KDF runs: %d for a live token id, %d for an unknown one — the failure path is an existence oracle",
			existing, missing)
	}

	// A revoked row must not be distinguishable either.
	if err := h.store.RevokeToken(t.Context(), "op"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if code, _, _ := bearerGet(t, ts.URL, "/api/v1/jobs",
		"exo_"+strconv.FormatInt(id, 10)+"_"+secret); code != http.StatusUnauthorized {
		t.Fatal("a revoked token must be 401")
	}
	if revoked := kdfRuns.Swap(0); revoked != missing {
		t.Fatalf("KDF runs: %d for a revoked row, %d for an unknown one", revoked, missing)
	}
}
