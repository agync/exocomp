package metrics

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDeriver stands in for the envelope: a deterministic subkey per
// (purpose, id), the same shape the real Derive returns. It is enough to prove
// the scrape token gate — the KDF itself is the envelope's concern.
type fakeDeriver struct{ seed string }

func (d fakeDeriver) Derive(purpose, id string) []byte {
	sum := sha256.Sum256([]byte(d.seed + "|" + purpose + ":" + id))
	return sum[:]
}

// TestMetricsScrapeRequiresBearer covers the scrape bearer gate: the endpoint
// shares the loopback state-backend listener that tofu-tier jobs can reach, so an
// unauthenticated scrape — the shape a job or a webhook-fronting caller sends —
// is refused, while a scrape carrying the derived bearer works.
func TestMetricsScrapeRequiresBearer(t *testing.T) {
	st := testStore(t)
	d := fakeDeriver{seed: "envelope"}
	h := Authenticated(New(st, "").Handler(), d)

	// The job-reachable path: no credential. Refused, and nothing leaks.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated scrape = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "exocomp_up") {
		t.Fatalf("a refused scrape served the gauges:\n%s", rec.Body.String())
	}
	if auth := rec.Header().Get("WWW-Authenticate"); !strings.Contains(auth, "Bearer") {
		t.Errorf("refusal omits a Bearer challenge: %q", auth)
	}

	// A wrong token is refused too — the compare is on the derived value.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer not-the-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token scrape = %d, want 401", rec.Code)
	}

	// The authorized scrape — Prometheus configured with the derived credential.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+ScrapeToken(d))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized scrape = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "exocomp_up 1") {
		t.Fatalf("authorized scrape missing gauges:\n%s", rec.Body.String())
	}
}

// TestScrapeTokenIsDeterministic pins the token to the derivation, so the
// daemon, doctor, and a recovery-time operator all compute the one Prometheus
// is configured with. A different seed is a different token.
func TestScrapeTokenIsDeterministic(t *testing.T) {
	a := ScrapeToken(fakeDeriver{seed: "envelope"})
	if a == "" {
		t.Fatal("scrape token is empty")
	}
	if a != ScrapeToken(fakeDeriver{seed: "envelope"}) {
		t.Fatal("scrape token is not stable for one envelope")
	}
	if a == ScrapeToken(fakeDeriver{seed: "other"}) {
		t.Fatal("two envelopes derived the same scrape token")
	}
}

// TestAuthenticatedFailsClosedWithoutDeriver proves a missing deriver never
// serves metrics in the clear: it refuses every scrape rather than opening up.
func TestAuthenticatedFailsClosedWithoutDeriver(t *testing.T) {
	st := testStore(t)
	h := Authenticated(New(st, "").Handler(), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer anything")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil-deriver scrape = %d, want 401", rec.Code)
	}
}
